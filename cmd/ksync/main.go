// Command ksync is a local-development sync loop for Kubernetes: it watches local
// kustomize directories and, on change, renders, diffs, and applies the affected
// app to a local cluster with ArgoCD-parity sync semantics (helm hooks, sync
// waves, prune, server-side apply, health assessment).
//
// Design decisions live in docs/ADR/.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/go-logr/logr"
	"golang.org/x/term"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"

	"github.com/motoki317/ksync/internal/build"
	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/loop"
	"github.com/motoki317/ksync/internal/render"
	"github.com/motoki317/ksync/internal/ui"
)

// defaultSyncTimeout bounds how long one app's sync may wait to converge.
// gitops-engine blocks until every health-gated wave is Healthy, so without a
// bound a single stuck workload (ErrImagePull, CrashLoop) hangs ksync forever.
// On expiry the sync fails with the names of the resources still unhealthy;
// in watch mode the scheduler then retries with backoff.
const defaultSyncTimeout = 5 * time.Minute

// The Milestone 1 CLI surface. Commands without an implementation yet are
// stubs; listing them all from day one fixes the command names early.
var subcommands = []struct {
	name, summary string
	run           func(args []string) error
}{
	{"watch", "watch app directories and render/diff/apply on change (the main loop)", runWatch},
	{"sync", "render and sync the given apps once", runSync},
	{"diff", "render and show the diff against live cluster state", nil},
	{"render", "render the given apps to stdout", runRender},
	{"destroy", "delete all tracked resources of the given apps", runDestroy},
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ksync:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return fmt.Errorf("no subcommand given")
	}
	for _, c := range subcommands {
		if args[0] == c.name {
			if c.run == nil {
				return fmt.Errorf("%s: not implemented yet", c.name)
			}
			return c.run(args[1:])
		}
	}
	usage(os.Stderr)
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: ksync <command> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	for _, c := range subcommands {
		fmt.Fprintf(w, "  %-8s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
}

// loadConfig adds the shared -f flag to fs, parses args, and loads the
// config; the remaining positional args are returned for the subcommand
// (usually app names). Subcommand-specific flags must be registered on fs
// before calling.
func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, []string, error) {
	path := fs.String("f", "ksync.yaml", "path to the ksync config file")
	names, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return nil, nil, err
	}
	return cfg, names, nil
}

// resolveContext picks the kubectl context a run targets — the explicit
// --context override, else the kubeconfig's current-context — and enforces
// ksync.yaml's allowedContexts. This is the safety gate: a current-context
// pointing at a cluster the config does not list is refused, not used.
func resolveContext(cfg *config.Config, override string) (string, error) {
	current, err := engine.CurrentContext()
	if err != nil {
		return "", err
	}
	return cfg.SelectContext(override, current)
}

// contextFlag registers the shared --context flag on a subcommand's flag set.
func contextFlag(fs *flag.FlagSet) *string {
	return fs.String("context", "", "kubectl context to target; must be listed in allowedContexts (default: current-context)")
}

// parseInterspersed parses fs allowing flags and positional args (app names) in
// any order — `sync app -timeout 5m` works as well as `sync -timeout 5m app`.
// Go's flag package stops at the first positional, which surprises developers
// who put the app name first; this permutes by re-parsing past each positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var names []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		names = append(names, args[0])
		args = args[1:]
	}
	return names, nil
}

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	offline := fs.Bool("offline-render", false, "render helm charts without live-cluster lookup (charts using helm `lookup` will not resolve)")
	kctx := contextFlag(fs)
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	kubeContext, err := resolveContext(cfg, *kctx)
	if err != nil {
		return err
	}
	renderOpts, cleanup, err := renderOptions(kubeContext, *offline)
	if err != nil {
		return err
	}
	defer cleanup()
	r := render.New(renderOpts)
	for i, app := range apps {
		res, err := r.Render(app.Path)
		if err != nil {
			return fmt.Errorf("app %s: %w", app.Name, err)
		}
		if i > 0 {
			fmt.Println("---")
		}
		yml, err := res.YAML()
		if err != nil {
			return fmt.Errorf("app %s: %w", app.Name, err)
		}
		if _, err := os.Stdout.Write(yml); err != nil {
			return err
		}
	}
	return nil
}

// setupLogging builds ksync's two loggers and silences the Kubernetes client
// noise. The app logger carries ksync's own build/sync/watch status; the quiet
// logger keeps only genuine errors and also backs klog, so client-go and
// gitops-engine's internal chatter ("Syncing", "Invalidated cluster", request
// throttling, …) never reaches the terminal. Logs go to stderr so `render`'s
// stdout stays clean.
func setupLogging(verbose bool) (app, engineLog logr.Logger) {
	v := 0
	if verbose {
		v = 1
	}
	app = ui.New(ui.Options{Writer: os.Stderr, Verbosity: v})
	// The engine and the routed client-go (klog) share one Error-only logger:
	// gitops-engine's per-sync chatter ("Syncing", "Tasks (dry-run)",
	// "Namespace already exists", …) and client-go's request noise are all
	// Info-level and dropped; only genuine failures reach the terminal.
	engineLog = ui.New(ui.Options{Writer: os.Stderr, Quiet: true})
	klog.SetLogger(engineLog)
	return app, engineLog
}

func runSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	prune := fs.Bool("prune", true, "delete tracked resources missing from the rendered output")
	timeout := fs.Duration("timeout", defaultSyncTimeout, "max time to wait for one app to converge before failing (0 = no limit)")
	maxParallel := fs.Int("max-parallel", runtime.NumCPU(), "how many apps may build, render, and sync concurrently (0 = no limit; default = CPU cores)")
	verbose := fs.Bool("v", false, "verbose: also log per-change tracing")
	offline := fs.Bool("offline-render", false, "render helm charts without live-cluster lookup (charts using helm `lookup` will not resolve)")
	kctx := contextFlag(fs)
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	apps = config.SortByNeeds(apps)
	kubeContext, err := resolveContext(cfg, *kctx)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, engineLog := setupLogging(*verbose)
	eng, err := engine.New(kubeContext, engineLog)
	if err != nil {
		return err
	}
	defer eng.Close()

	out := ui.NewColors(os.Stderr)
	renderOpts, cleanup, err := renderOptions(kubeContext, *offline)
	if err != nil {
		return err
	}
	defer cleanup()
	r := render.New(renderOpts)
	prog := newProgress(os.Stderr, out, apps)
	buildFn := makeBuildFunc(cfg, prog, kubeContext)
	// A whole-stack run gets a plan up front, the summary block pinned live to
	// the bottom (updating as apps finish), and the same block committed on
	// completion; a single-app run already says it all in its one line.
	multi := len(apps) > 1
	nameW := nameColWidth(apps)
	if multi {
		printPlan(os.Stderr, out, apps, kubeContext)
	}
	// Independent apps build/render/sync concurrently; the needs DAG still
	// serializes a dependent after the apps it needs (watch mode already does
	// this — one-shot sync should not be slower than the loop's initial pass).
	var mu sync.Mutex
	var agg loop.SyncStats
	var synced int
	var degradedApps []string
	started := time.Now()
	var footer *ui.Footer
	if multi {
		// The ticker calls render from another goroutine, so read the tally under
		// the same lock the run goroutines write it with.
		footer = ui.StartFooter(os.Stderr, out, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return summaryLines(out, len(apps), synced, agg, degradedApps, time.Since(started), false)
		})
	}
	err = runByNeeds(ctx, apps, *maxParallel, func(ctx context.Context, app config.App) error {
		// Per-app wall clock: build + render + apply + health gate, the same
		// end-to-end span the watch loop reports on its 🚢 line. Captured per
		// invocation since apps run concurrently.
		start := time.Now()
		results, degraded, err := syncOneApp(ctx, r, eng, buildFn, prog, app, *prune, *timeout, *maxParallel)
		if err != nil {
			prog.finish(app.Name, "") // remove the live group; the error is returned and printed at the top level
			return err
		}
		took := time.Since(start)
		stats := syncStats(results)
		stats.Degraded = len(degraded)
		mu.Lock()
		synced++
		agg.Applied += stats.Applied
		agg.Pruned += stats.Pruned
		agg.Failed += stats.Failed
		agg.Degraded += stats.Degraded
		if stats.Degraded > 0 {
			degradedApps = append(degradedApps, app.Name)
		}
		mu.Unlock()
		// Commit after tallying so the ✓ line and the footer's bumped count land
		// together (Finish repaints the footer as it writes the line).
		prog.finish(app.Name, summaryBlock(out, app.Name, results, degraded, took, nameW))
		return nil
	})
	footer.Stop()
	if multi {
		mu.Lock()
		final := summaryLines(out, len(apps), synced, agg, degradedApps, time.Since(started), true)
		mu.Unlock()
		printSummaryBlock(os.Stderr, final)
	}
	return err
}

// syncOneApp builds (by group batch), renders, injects built image tags, and
// applies one app, returning the per-resource sync results and the names of any
// degraded resources. Build precedes render so the applied manifests always
// reference images that exist in the local daemon. The build/import rows and the
// deploy row land in the app's pipeline; the caller commits it after tallying so
// the live footer's count tracks the committed lines.
func syncOneApp(ctx context.Context, r *render.Renderer, eng *engine.Engine, buildFn loop.BuildFunc, prog *progress, app config.App, prune bool, timeout time.Duration, maxParallel int) ([]common.ResourceSyncResult, []string, error) {
	all := make([]int, len(app.Build))
	for i := range all {
		all[i] = i
	}
	// Independent build batches run concurrently (up to maxParallel), the same
	// fan-out the watch loop uses — a multi-image app's builds are not serialized.
	built, err := loop.BuildAll(ctx, app, all, buildFn, maxParallel)
	if err != nil {
		return nil, nil, fmt.Errorf("app %s: build failed: %w", app.Name, err)
	}
	images := make([]render.Image, 0, len(built))
	for j := range app.Build {
		if tag, ok := built[j]; ok {
			images = append(images, render.Image{Name: app.Build[j].Image, NewTag: tag})
		}
	}
	res, err := r.Render(app.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("app %s: %w", app.Name, err)
	}
	if len(images) > 0 {
		if err := res.SetImages(images); err != nil {
			return nil, nil, fmt.Errorf("app %s: %w", app.Name, err)
		}
	}
	deploy := prog.pipeline(app.Name).Deploy()
	deploy.Start()
	syncCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()
	results, err := eng.Sync(syncCtx, app.Name, res.Objects, engine.SyncOptions{Prune: prune, Namespace: app.Namespace, OnWait: deployWait(deploy)})
	deploy.Done(err)
	if err != nil {
		return nil, nil, fmt.Errorf("app %s: %w", app.Name, err)
	}
	return results, eng.AppDegraded(app.Name, app.Namespace, res.Objects), nil
}

// deployWait reports the health gate's progress on the app's deploy row: each
// poll that is still waiting updates the not-ready count. An already-healthy
// sync never calls back, so the row simply finishes with its elapsed time.
func deployWait(deploy *ui.Stage) func([]string) {
	return func(pending []string) {
		deploy.SetTail(fmt.Sprintf("waiting for health  %d not ready", len(pending)))
	}
}

// runByNeeds runs fn for every app, up to maxParallel concurrently, starting an
// app only once every app it needs has finished. apps must be topologically
// sorted (config.SortByNeeds). It returns the first error and, on any error,
// cancels the derived context so apps not yet started are skipped.
func runByNeeds(ctx context.Context, apps []config.App, maxParallel int, fn func(context.Context, config.App) error) error {
	if len(apps) == 0 {
		return nil
	}
	// 0 (or negative) means no limit; a cap above the app count is the same as
	// the count, so the semaphore never holds more slots than can ever be used.
	if maxParallel < 1 || maxParallel > len(apps) {
		maxParallel = len(apps)
	}
	done := make(map[string]chan struct{}, len(apps))
	for _, a := range apps {
		done[a.Name] = make(chan struct{})
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error
	for _, a := range apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[a.Name])
			for _, dep := range a.Needs {
				ch, ok := done[dep]
				if !ok {
					continue // need is outside this run's app set
				}
				select {
				case <-ch:
				case <-ctx.Done():
					return
				}
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if err := fn(ctx, a); err != nil {
				errOnce.Do(func() { firstErr = err; cancel() })
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// withTimeout bounds ctx by d, or returns it unchanged (with a no-op cancel)
// when d is non-positive — the documented "no limit" escape hatch.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	prune := fs.Bool("prune", true, "delete tracked resources missing from the rendered output")
	debounce := fs.Duration("debounce", 200*time.Millisecond, "quiet period after the last change before re-rendering")
	maxParallel := fs.Int("max-parallel", runtime.NumCPU(), "how many apps may build, render, and sync concurrently (0 = no limit; default = CPU cores)")
	timeout := fs.Duration("timeout", defaultSyncTimeout, "max time to wait for one app to converge before retrying (0 = no limit)")
	verbose := fs.Bool("v", false, "verbose: also log per-change tracing")
	offline := fs.Bool("offline-render", false, "render helm charts without live-cluster lookup (charts using helm `lookup` will not resolve)")
	kctx := contextFlag(fs)
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	kubeContext, err := resolveContext(cfg, *kctx)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log, engineLog := setupLogging(*verbose)
	renderOpts, cleanup, err := renderOptions(kubeContext, *offline)
	if err != nil {
		return err
	}
	defer cleanup()
	eng, err := engine.New(kubeContext, engineLog)
	if err != nil {
		return err
	}
	defer eng.Close()

	out := ui.NewColors(os.Stderr)
	nsByApp := make(map[string]string, len(apps))
	for _, a := range apps {
		nsByApp[a.Name] = a.Namespace
	}
	prog := newProgress(os.Stderr, out, apps)
	// The loop logs build/sync state itself; the per-app timeout keeps one
	// stuck workload from holding a scheduler slot forever — on expiry the
	// sync fails and the scheduler retries it with backoff. The deploy row of the
	// app's pipeline shows the health-gate wait; on success the Report hook
	// commits the group, on failure the OnError hook removes it.
	syncFn := func(ctx context.Context, app string, objs []*unstructured.Unstructured) (loop.SyncStats, error) {
		ctx, cancel := withTimeout(ctx, *timeout)
		defer cancel()
		deploy := prog.pipeline(app).Deploy()
		deploy.Start()
		results, err := eng.Sync(ctx, app, objs, engine.SyncOptions{Prune: *prune, Namespace: nsByApp[app], OnWait: deployWait(deploy)})
		deploy.Done(err)
		stats := syncStats(results)
		if err == nil {
			stats.Degraded = len(eng.AppDegraded(app, nsByApp[app], objs))
		}
		return stats, err
	}
	// Frame the startup convergence like a `ksync sync` run — a Plan, a live
	// Summary footer, then the committed Summary once every app has synced once
	// — after which incremental syncs just stream. Stop the footer on exit so a
	// Ctrl-C mid-convergence never leaves it pinned.
	reporter := startWatchReporter(os.Stderr, out, prog, apps, kubeContext)
	defer reporter.stop()
	return loop.Run(ctx, apps, syncFn, loop.Options{
		Debounce:    *debounce,
		MaxParallel: *maxParallel,
		Render:      renderOpts,
		Build:       makeBuildFunc(cfg, prog, kubeContext),
		Log:         log,
		Report:      reporter.report,
		// A build/render failure (or a failed sync) ends the run without a Report;
		// remove the live group so a retry starts fresh rather than stacking rows.
		OnError: func(app string, _ error) { prog.finish(app, "") },
		Resync:  resyncOnEnter(ctx, log),
	})
}

// resyncOnEnter returns a channel that fires whenever the user presses Enter,
// the watch loop's manual "redeploy everything" control. It is active only when
// stdin is an interactive terminal — under a pipe or a process manager there is
// no keyboard, so it returns nil (the loop treats that as "never"). The reader
// goroutine ends with the process; Ctrl-C remains the way to quit.
func resyncOnEnter(ctx context.Context, log logr.Logger) <-chan struct{} {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	log.Info("watching; press Enter to resync all apps, Ctrl-C to quit")
	ch := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			select {
			case ch <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// watchReporter renders the watch loop's per-app sync lines and, for a
// whole-stack watch, frames the startup convergence the way a `ksync sync` run
// is framed: a Plan up front, a live Summary footer pinned with the running
// tally, and the committed Summary block once every app has synced once. After
// that first convergence it streams incremental syncs with no footer — a
// per-keystroke summary block would be noise. A single-app watch skips all of
// it (footer == nil) and just streams the one ship line, like a one-app sync.
type watchReporter struct {
	w       io.Writer
	out     ui.Colors
	prog    *progress
	total   int
	nameW   int
	started time.Time

	mu           sync.Mutex
	agg          loop.SyncStats
	degradedApps []string
	seen         map[string]bool // apps that have completed their first sync
	converged    bool
	footer       *ui.Footer
}

// startWatchReporter prints the Plan and pins the live Summary footer for a
// multi-app watch; a single-app watch gets an inert reporter (no Plan/footer).
func startWatchReporter(w io.Writer, out ui.Colors, prog *progress, apps []config.App, kubeContext string) *watchReporter {
	r := &watchReporter{w: w, out: out, prog: prog, total: len(apps), nameW: nameColWidth(apps), started: time.Now(), seen: make(map[string]bool, len(apps))}
	if r.total <= 1 {
		return r
	}
	printPlan(w, out, apps, kubeContext)
	r.footer = ui.StartFooter(w, out, func() []string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return summaryLines(out, r.total, len(r.seen), r.agg, r.degradedApps, time.Since(r.started), false)
	})
	return r
}

// report commits one completed app sync (the loop's Report hook): it Finishes
// the app's pipeline with the shared ship-emoji apply line, and on the run that
// completes the initial convergence commits the Summary block and removes the
// footer.
func (r *watchReporter) report(app string, stats loop.SyncStats, took time.Duration) {
	line := appSyncLine(r.out, app, stats, took, r.nameW)
	done := false
	r.mu.Lock()
	// Tally each app's FIRST sync only, so the Summary reflects one convergence
	// rather than every later edit; convergence is when all apps have synced.
	// Idle (no app dirty/running) is reached only when every app has succeeded,
	// so a stuck app holds the footer open instead of committing a false "done".
	if r.footer != nil && !r.converged && !r.seen[app] {
		r.seen[app] = true
		r.agg.Applied += stats.Applied
		r.agg.Pruned += stats.Pruned
		r.agg.Failed += stats.Failed
		r.agg.Degraded += stats.Degraded
		if stats.Degraded > 0 {
			r.degradedApps = append(r.degradedApps, app)
		}
		if len(r.seen) == r.total {
			r.converged = true
			done = true
		}
	}
	r.mu.Unlock()

	r.prog.finish(app, line+"\n")

	if done {
		r.footer.Stop()
		r.mu.Lock()
		final := summaryLines(r.out, r.total, len(r.seen), r.agg, r.degradedApps, time.Since(r.started), true)
		r.mu.Unlock()
		printSummaryBlock(r.w, final)
	}
}

// stop removes the live footer; idempotent, so the deferred call after the loop
// exits is harmless when convergence already stopped it. It matters when Ctrl-C
// arrives mid-convergence — without it the footer stays pinned.
func (r *watchReporter) stop() { r.footer.Stop() }

// progress coordinates the per-app pipelines (ui.Pipeline) that group an app's
// build, import, and deploy stages under one header. One pipeline per app run:
// the build hook attaches the build/import rows, the deploy attaches its row,
// and the orchestrator commits the group with the app's summary line. Keyed by
// app name — an app never runs concurrently with itself (one-shot sync runs each
// app's fn once; the watch scheduler serializes per app) — so the key names
// exactly one live pipeline.
type progress struct {
	w      io.Writer
	colors ui.Colors
	expand map[string]bool // app -> has builds: show the full tree, never collapse

	mu    sync.Mutex
	pipes map[string]*ui.Pipeline
}

func newProgress(w io.Writer, colors ui.Colors, apps []config.App) *progress {
	expand := make(map[string]bool, len(apps))
	for _, a := range apps {
		expand[a.Name] = len(a.Build) > 0
	}
	return &progress{w: w, colors: colors, expand: expand, pipes: make(map[string]*ui.Pipeline)}
}

// pipeline returns the app's live pipeline, creating it (with a pending Deploy
// row, so the deploy shows as upcoming work while builds run) on first use this
// run. The build hook and the deploy share the one returned per app.
func (p *progress) pipeline(app string) *ui.Pipeline {
	p.mu.Lock()
	defer p.mu.Unlock()
	pipe := p.pipes[app]
	if pipe == nil {
		pipe = ui.StartPipeline(p.w, p.colors, app, p.expand[app])
		pipe.Deploy() // pending; rendered beneath the builds
		p.pipes[app] = pipe
	}
	return pipe
}

// finish commits the app's pipeline (summary printed in place of the live group,
// or the group removed silently when summary is empty — a failed run whose error
// surfaces elsewhere) and clears it so the next run starts fresh.
func (p *progress) finish(app, summary string) {
	p.mu.Lock()
	pipe := p.pipes[app]
	delete(p.pipes, app)
	p.mu.Unlock()
	if pipe != nil {
		pipe.Finish(summary)
		return
	}
	// No live group for this app (a path that opened none); still surface the
	// summary above any other live block so a committed line is never lost.
	if summary != "" {
		ui.WriteLine(p.w, summary)
	}
}

// makeBuildFunc composes building an image with loading it into the cluster, so
// the same path serves one-shot sync and the watch loop. Each build batch adds a
// Build row (and, when the resulting image is fresh, an Import row) to its app's
// pipeline; the verbose command log is shown only when the command fails. The
// load step is a no-op unless the config sets imageLoad (daemon-shared clusters
// need nothing).
func makeBuildFunc(cfg *config.Config, prog *progress, kubeContext string) loop.BuildFunc {
	groupCmd := make(map[string]string, len(cfg.BuildGroups))
	for _, g := range cfg.BuildGroups {
		groupCmd[g.Name] = g.Command
	}
	// imported remembers the refs already made visible to the cluster this
	// session, so a rebuild that yields an unchanged ref (a save that does not
	// change the image — a comment, a reformat — produces the same fingerprint)
	// skips the import. A content-addressed ref names exactly one image; once
	// imported it stays in the cluster's store (a deployed image is not GC'd),
	// so re-importing it is pure waste — and on k3d/kind that waste is seconds.
	var mu sync.Mutex
	imported := map[string]bool{}
	// One Loader shared across the parallel per-app builds. It serializes loads
	// unless the config marks the command concurrency-safe (allowParallel) —
	// k3d image import, the unsafe default case, races on a shared tools node +
	// tarball and silently drops images (see build.Loader).
	loader := &build.Loader{Command: cfg.ImageLoad.Command, Parallel: cfg.ImageLoad.Parallel(), KubeContext: kubeContext}
	return func(ctx context.Context, app string, builds []config.Build) ([]string, error) {
		if len(builds) == 0 {
			return nil, nil
		}
		pipe := prog.pipeline(app)
		// A batch is either one ungrouped entry or all the dirty members of one
		// group; builds[0].Group tells which, since the loop never mixes them. The
		// app heads the group, so an ungrouped label is just the image name and a
		// group's label is the group name plus its member count (how many images
		// this one bake produces).
		label := imageName(builds[0].Image)
		if group := builds[0].Group; group != "" {
			label = fmt.Sprintf("%s (%d)", group, len(builds))
		}
		stage := pipe.Build(label)
		var refs []string
		var err error
		if group := builds[0].Group; group != "" {
			refs, err = (&build.Builder{Output: stage, KubeContext: kubeContext}).BuildGroup(ctx, groupCmd[group], builds)
		} else {
			var ref string
			ref, err = (&build.Builder{Output: stage, KubeContext: kubeContext}).Build(ctx, builds[0])
			refs = []string{ref}
		}
		stage.Done(err)
		if err != nil {
			return nil, err
		}
		if cfg.ImageLoad.Command != "" {
			mu.Lock()
			fresh := make([]string, 0, len(refs))
			for _, ref := range refs {
				if !imported[ref] {
					fresh = append(fresh, ref)
				}
			}
			mu.Unlock()
			if len(fresh) > 0 {
				stage := pipe.Import(label)
				err := loader.Load(ctx, stage, fresh)
				stage.Done(err)
				if err != nil {
					return nil, err
				}
				mu.Lock()
				for _, ref := range fresh {
					imported[ref] = true
				}
				mu.Unlock()
			}
		}
		return refs, nil
	}
}

// imageName is the short, human-facing name of an image ref for progress
// labels: the last path segment without the tag (ghcr.io/org/ns-auth-dev →
// ns-auth-dev).
func imageName(image string) string {
	if i := strings.LastIndexByte(image, ':'); i > strings.LastIndexByte(image, '/') {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, '/'); i >= 0 {
		image = image[i+1:]
	}
	return image
}

func runDestroy(args []string) error {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm deleting every tracked resource of the selected apps")
	timeout := fs.Duration("timeout", defaultSyncTimeout, "max time to wait for one app's resources to delete (0 = no limit)")
	kctx := contextFlag(fs)
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	kubeContext, err := resolveContext(cfg, *kctx)
	if err != nil {
		return err
	}
	if !*yes {
		all := make([]string, len(apps))
		for i, a := range apps {
			all[i] = a.Name
		}
		return fmt.Errorf("destroy deletes every tracked resource of: %s — re-run with -yes to confirm", strings.Join(all, ", "))
	}
	// Dependents go down before their dependencies.
	apps = config.SortByNeeds(apps)
	slices.Reverse(apps)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, engineLog := setupLogging(false)
	eng, err := engine.New(kubeContext, engineLog)
	if err != nil {
		return err
	}
	defer eng.Close()

	out := ui.NewColors(os.Stderr)
	nameW := nameColWidth(apps)
	for _, app := range apps {
		// Destroy is a sync to an empty target set: prune removes everything
		// the tracking label scopes to this app, and nothing else.
		syncCtx, cancel := withTimeout(ctx, *timeout)
		start := time.Now()
		results, err := eng.Sync(syncCtx, app.Name, nil, engine.SyncOptions{Prune: true})
		cancel()
		if err != nil {
			return fmt.Errorf("app %s: destroy: %w", app.Name, err)
		}
		printSummary(os.Stderr, out, app.Name, results, nil, time.Since(start), nameW)
	}
	return nil
}

// printSummary condenses one app's sync into a single status line — the count
// of resources applied/pruned, failures called out in red — and lists only the
// resources that failed or ran as hooks. The full per-resource dump is noise on
// a healthy sync (which is the common case); the line is what a developer scans.
// syncStats reduces the engine's per-object results to the apply summary shared
// by `ksync sync` (printSummary) and `ksync watch` (the loop's status line), so
// both report applied/pruned/failed counts the same way.
func syncStats(results []common.ResourceSyncResult) loop.SyncStats {
	var s loop.SyncStats
	for _, res := range results {
		switch res.Status {
		case common.ResultCodePruned:
			s.Pruned++
		case common.ResultCodeSyncFailed:
			s.Failed++
		default:
			s.Applied++
		}
	}
	return s
}

// summaryBlock renders one app's committed sync block: the ship-emoji apply
// line, preceded by a line per failed resource (✗) and per degraded one (⚠). It
// is what a pipeline commits to scrollback in place of its live group, and what
// destroy prints directly.
func summaryBlock(c ui.Colors, app string, results []common.ResourceSyncResult, degraded []string, took time.Duration, nameW int) string {
	var b strings.Builder
	s := syncStats(results)
	s.Degraded = len(degraded)
	for _, res := range results {
		if res.Status == common.ResultCodeSyncFailed {
			fmt.Fprintf(&b, "  %s %s: %s\n", c.Red("✗"), res.ResourceKey.String(), res.Message)
		}
	}
	// A resource applied cleanly but is broken at runtime (CrashLoop, failed
	// Job): the sync succeeded, yet the developer needs to see it. Listed under
	// the app line, distinct from a sync ✗.
	for _, line := range degraded {
		fmt.Fprintf(&b, "  %s %s\n", c.Yellow("⚠"), c.Dim(line))
	}
	fmt.Fprintf(&b, "%s\n", appSyncLine(c, app, s, took, nameW))
	return b.String()
}

// printSummary writes one app's summary block above any live block (destroy has
// no pipeline of its own to commit).
func printSummary(w io.Writer, c ui.Colors, app string, results []common.ResourceSyncResult, degraded []string, took time.Duration, nameW int) {
	ui.WriteLine(w, summaryBlock(c, app, results, degraded, took, nameW))
}

// appSyncLine renders one app's completed-sync status line — the committed form
// of its pipeline's deploy, in the same 🚢 Deploy style as the live row so a
// scrollback line and a live one read alike: a health symbol (✓ applied &
// healthy · ⚠ applied but a resource is degraded · ✗ a sync task failed), the 🚢
// icon with the explicit "Deploy" kind, the app name, and a dim summary of what
// changed. nameW (the run's widest app name, 0 for a single-app run) pads the
// name so the change-summary — and, when the summaries are equal width, the
// trailing duration — line up across the streamed per-app lines. took, when > 0,
// is appended (the per-app end-to-end timing); a zero duration omits it.
func appSyncLine(c ui.Colors, app string, s loop.SyncStats, took time.Duration, nameW int) string {
	parts := []string{fmt.Sprintf("%d applied", s.Applied)}
	if s.Pruned > 0 {
		parts = append(parts, fmt.Sprintf("%d pruned", s.Pruned))
	}
	if s.Failed > 0 {
		parts = append(parts, c.Red(fmt.Sprintf("%d failed", s.Failed)))
	}
	if s.Degraded > 0 {
		parts = append(parts, c.Yellow(fmt.Sprintf("%d degraded", s.Degraded)))
	}
	symbol := c.Green("✓")
	if s.Degraded > 0 {
		symbol = c.Yellow("⚠")
	}
	if s.Failed > 0 {
		symbol = c.Red("✗")
	}
	// k8s names are ASCII, so len is the display width; pad with plain spaces
	// after the colored name so the next column starts at a fixed offset.
	name := c.Bold(app)
	if pad := nameW - len(app); pad > 0 {
		name += strings.Repeat(" ", pad)
	}
	line := fmt.Sprintf("%s %s Deploy %s  %s", symbol, ui.IconDeploy, name, c.Dim(strings.Join(parts, ", ")))
	if took > 0 {
		line += "  " + ui.Elapsed(c, took)
	}
	return line
}

// nameColWidth is the app-name column width for the streamed per-app summary
// lines: the run's widest name, so they align — or 0 for a single-app run, which
// needs no padding (and reads cleanest left-tight).
func nameColWidth(apps []config.App) int {
	if len(apps) <= 1 {
		return 0
	}
	w := 0
	for _, a := range apps {
		if len(a.Name) > w {
			w = len(a.Name)
		}
	}
	return w
}

// printPlan opens a whole-stack sync with a titled overview: how many apps, the
// target context, and the app names — so the developer sees the scope before
// the per-app lines start streaming. The trailing blank line sets it apart from
// the streamed log that follows.
func printPlan(w io.Writer, c ui.Colors, apps []config.App, kubeContext string) {
	names := make([]string, len(apps))
	for i, a := range apps {
		names[i] = a.Name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", c.Bold("Plan"))
	fmt.Fprintf(&b, "  %s %s %s\n", fmt.Sprintf("%d apps", len(apps)), c.Dim("→"), c.Bold(kubeContext))
	fmt.Fprintf(&b, "  %s\n\n", c.Dim(strings.Join(names, " ")))
	ui.WriteLine(w, b.String())
}

// summaryLines renders the titled run-summary block (vitest-style right-aligned
// labels): apps synced, any failed/degraded apps, and the elapsed time. It is
// both the live footer (final=false → "k/N synced", refreshed each tick) and
// the committed close (final=true → "N synced", with degraded apps named).
// Returns the block's lines without trailing newlines.
func summaryLines(c ui.Colors, total, synced int, agg loop.SyncStats, degradedApps []string, elapsed time.Duration, final bool) []string {
	type row struct{ label, value string }
	appsVal := fmt.Sprintf("%d/%d synced", synced, total)
	if final {
		appsVal = fmt.Sprintf("%d synced", synced)
	}
	rows := []row{{"Apps", c.Bold(appsVal)}}
	if agg.Failed > 0 {
		rows = append(rows, row{"Failed", c.Red(fmt.Sprintf("%d failed", agg.Failed))})
	}
	if agg.Degraded > 0 {
		v := c.Yellow(fmt.Sprintf("%d degraded", agg.Degraded))
		// Name the degraded apps only in the committed block — the live footer
		// stays short so a long list cannot wrap and break the pinned block.
		if final && len(degradedApps) > 0 {
			v += c.Dim("  " + strings.Join(degradedApps, ", "))
		}
		rows = append(rows, row{"Degraded", v})
	}
	rows = append(rows, row{"Duration", ui.Duration(elapsed)})

	width := 0
	for _, ln := range rows {
		if len(ln.label) > width {
			width = len(ln.label)
		}
	}
	// Lead with a blank line so the block is set off from the log above it — both
	// while pinned live (this footer) and when committed (printSummaryBlock), so
	// the spacing is identical in both states.
	lines := []string{"", c.Bold("Summary")}
	for _, ln := range rows {
		// Right-align the label (padding added before color-wrapping, so the
		// columns line up regardless of escape codes), value after a 2-space gap.
		lines = append(lines, fmt.Sprintf("  %s  %s", c.Dim(fmt.Sprintf("%*s", width, ln.label)), ln.value))
	}
	return lines
}

// printSummaryBlock commits the final summary block to scrollback. The leading
// blank line comes from summaryLines, matching the live footer that was just
// cleared.
func printSummaryBlock(w io.Writer, lines []string) {
	ui.WriteLine(w, strings.Join(lines, "\n")+"\n")
}
