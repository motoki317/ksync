// Command ksync is a local-development sync loop for Kubernetes: it watches local
// kustomize directories and, on change, renders, diffs, and applies the affected
// app to a local cluster with ArgoCD-parity sync semantics (helm hooks, sync
// waves, prune, server-side apply, health assessment).
//
// Design decisions live in docs/ADR/.
package main

import (
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
	{"images", "list the container images the given apps deploy (canonical refs, for cache scoping)", runImages},
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
	maxParallel := fs.Int("max-parallel", runtime.NumCPU(), "how many apps to render concurrently (0 = one worker per app)")
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
	// Render and serialize concurrently — each is independent, and both the
	// per-chart helm dry-runs and the YAML marshal are the per-app cost.
	yamls, err := renderConcurrently(apps, *maxParallel, func(app config.App) ([]byte, error) {
		res, err := r.Render(app.Path)
		if err != nil {
			return nil, fmt.Errorf("app %s: %w", app.Name, err)
		}
		yml, err := res.YAML()
		if err != nil {
			return nil, fmt.Errorf("app %s: %w", app.Name, err)
		}
		return yml, nil
	})
	if err != nil {
		return err
	}
	for i, yml := range yamls {
		if i > 0 {
			fmt.Println("---")
		}
		if _, err := os.Stdout.Write(yml); err != nil {
			return err
		}
	}
	return nil
}

// renderConcurrently runs fn over apps with bounded concurrency, returning the
// results in app order. Per-app rendering shells out to helm once per chart
// release and each release waits on a live-cluster dry-run, so the work is
// I/O-bound, overlaps well, and the wall-clock floor is the slowest single app
// (rendering 20+ apps one at a time is what made `render`/`images` take ~10s).
// The Renderer is concurrency-safe — the watch/sync loop already renders apps
// in parallel this way. maxParallel <= 0 means one worker per app.
func renderConcurrently[T any](apps []config.App, maxParallel int, fn func(config.App) (T, error)) ([]T, error) {
	out := make([]T, len(apps))
	errs := make([]error, len(apps))
	if maxParallel <= 0 || maxParallel > len(apps) {
		maxParallel = len(apps)
	}
	if maxParallel < 1 {
		maxParallel = 1
	}
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i := range apps {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			v, err := fn(apps[i])
			if err != nil {
				errs[i] = err
				return
			}
			out[i] = v
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return out, err
		}
	}
	return out, nil
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
	force := fs.Bool("force", false, "re-run hooks even when manifests are unchanged (re-applies PostSync Jobs; a failed hook is retried regardless)")
	timeout := fs.Duration("timeout", defaultSyncTimeout, "max time to wait for one app to converge before failing (0 = no limit)")
	maxParallel := fs.Int("max-parallel", runtime.NumCPU(), "how many apps may build, render, and sync concurrently (0 = no limit; default = CPU cores)")
	verbose := fs.Bool("v", false, "verbose: also log per-change tracing")
	offline := fs.Bool("offline-render", false, "render helm charts without live-cluster lookup (charts using helm `lookup` will not resolve)")
	var images stringSlice
	fs.Var(&images, "image", "deploy a pre-built image instead of building it: IMAGE=REF (repeatable; also via "+overrideEnv+")")
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

	appLog, engineLog := setupLogging(*verbose)
	overrides, err := imageOverrides(cfg, images, appLog)
	if err != nil {
		return err
	}
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
	buildFn := makeBuildFunc(ctx, cfg, prog, kubeContext)
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
	// Builds run ahead of the needs DAG: an image is local, so it can build
	// while the apps it depends on still deploy. The deploy phase below awaits
	// each app's own build, so nothing applies an unbuilt image — but no build
	// waits behind a deploy, so a one-time sync is never slower than the loop's
	// startup pass (which does the same; ADR 20260616-eager-build-ahead).
	buildCtx, buildCancel := context.WithCancel(ctx)
	awaitBuild, waitBuilds := startEagerBuilds(buildCtx, apps, buildFn, *maxParallel, overrides)
	defer waitBuilds()
	defer buildCancel()
	err = runByNeeds(ctx, apps, *maxParallel, func(ctx context.Context, app config.App) error {
		bo := awaitBuild(app.Name)
		if bo.err != nil {
			prog.finish(app.Name, nil)
			return fmt.Errorf("app %s: build failed: %w", app.Name, bo.err)
		}
		results, degraded, err := deployApp(ctx, r, eng, prog, app, bo.tags, overrides, *prune, *force, *timeout)
		if err != nil {
			prog.finish(app.Name, nil) // remove the live group; the error is returned and printed at the top level
			return err
		}
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
		// together (Finish repaints the footer as it writes the block). The deploy
		// time on the committed row is the deploy stage's own (read from the
		// pipeline), so per-app build and apply times both survive the run.
		summary, symbol := applyParts(out, stats)
		prog.finish(app.Name, &ui.CommitInfo{Summary: summary, Symbol: symbol, Above: failureLines(out, results, degraded), NameW: nameW})
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

// buildOutcome is one app's eager build result, published once its build
// goroutine finishes. tags is nil when err is set.
type buildOutcome struct {
	tags map[int]string
	err  error
}

// startEagerBuilds launches one build goroutine per build-app immediately,
// bounded by maxParallel and independent of the needs DAG — an image is local
// (docker build + load into the cluster store), so it can build while the apps
// it depends on are still deploying, instead of waiting behind them. Entries
// whose image is in overrides are skipped (the supplied ref is injected at
// deploy, not built). It returns `await`, which blocks for one app's build
// outcome (the deploy phase calls it so an unbuilt image is never applied; an
// app with nothing to build returns a zero outcome at once), and `wait`, which
// drains the goroutines on shutdown.
func startEagerBuilds(ctx context.Context, apps []config.App, buildFn loop.BuildFunc, maxParallel int, overrides map[string]render.Image) (await func(string) buildOutcome, wait func()) {
	done := make(map[string]chan struct{}, len(apps))
	outcomes := make(map[string]buildOutcome, len(apps))
	var mu sync.Mutex
	var sem chan struct{}
	if maxParallel > 0 {
		sem = make(chan struct{}, maxParallel)
	}
	var wg sync.WaitGroup
	for _, a := range apps {
		var todo []int
		for j := range a.Build {
			if _, ov := overrides[a.Build[j].Image]; !ov {
				todo = append(todo, j)
			}
		}
		if len(todo) == 0 {
			continue // build-less, or every image overridden
		}
		ch := make(chan struct{})
		done[a.Name] = ch
		app := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(ch)
			if sem != nil {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					mu.Lock()
					outcomes[app.Name] = buildOutcome{err: ctx.Err()}
					mu.Unlock()
					return
				}
				defer func() { <-sem }()
			}
			// Independent build batches within the app run concurrently too (the
			// same fan-out the watch loop uses), bounded by maxParallel.
			tags, err := loop.BuildAll(ctx, app, todo, buildFn, maxParallel)
			mu.Lock()
			outcomes[app.Name] = buildOutcome{tags: tags, err: err}
			mu.Unlock()
		}()
	}
	await = func(app string) buildOutcome {
		ch, ok := done[app]
		if !ok {
			return buildOutcome{} // app has no build
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return buildOutcome{err: ctx.Err()}
		}
		mu.Lock()
		defer mu.Unlock()
		return outcomes[app]
	}
	return await, wg.Wait
}

// deployApp renders one app, injects its image refs (built dev tags and/or
// supplied overrides), and applies it — returning the per-resource sync results
// and the names of any degraded resources. The build ran ahead of this
// (startEagerBuilds) and the caller awaited it, so the applied manifests always
// reference images that exist. The deploy row lands in the app's pipeline; the
// caller commits it after tallying so the live footer's count tracks the
// committed lines.
func deployApp(ctx context.Context, r *render.Renderer, eng *engine.Engine, prog *progress, app config.App, tags map[int]string, overrides map[string]render.Image, prune, force bool, timeout time.Duration) ([]common.ResourceSyncResult, []string, error) {
	images := make([]render.Image, 0, len(app.Build))
	for j := range app.Build {
		if ov, ok := overrides[app.Build[j].Image]; ok {
			images = append(images, ov)
		} else if tag, ok := tags[j]; ok {
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
	results, err := eng.Sync(syncCtx, app.Name, res.Objects, engine.SyncOptions{Prune: prune, Force: force, Namespace: app.Namespace, OnWait: deployWait(deploy)})
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
	auto := fs.Bool("auto", false, "rebuild and redeploy automatically on every change, skipping the confirmation prompt")
	verbose := fs.Bool("v", false, "verbose: also log per-change tracing")
	offline := fs.Bool("offline-render", false, "render helm charts without live-cluster lookup (charts using helm `lookup` will not resolve)")
	var images stringSlice
	fs.Var(&images, "image", "deploy a pre-built image instead of building it: IMAGE=REF (repeatable; also via "+overrideEnv+")")
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

	// A cancelable context derived from the signal context, so the confirmation
	// picker can quit the run on Ctrl-C (in raw mode the terminal delivers no
	// SIGINT, so the picker calls cancel itself).
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	log, engineLog := setupLogging(*verbose)
	overrides, err := imageOverrides(cfg, images, log)
	if err != nil {
		return err
	}
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
	// By default ksync holds incremental changes for an explicit build/deploy
	// choice via the confirmation picker; -auto (or a non-interactive stdin, which
	// has no one to answer) keeps the classic auto-rebuild loop. The gate needs
	// both a readable stdin (raw keystrokes) and a stderr terminal (to draw on).
	var gate *loop.Gate
	if !*auto && interactiveTerminal() {
		gate = newBuildGate(ctx, cancel, os.Stderr, out)
		log.Info("Watching; on change you'll be asked what to rebuild — Ctrl-C to quit")
	} else {
		log.Info("Watching; rebuilding automatically on change — Ctrl-C to quit")
	}

	// Frame the startup convergence like a `ksync sync` run — a Plan, a live
	// Summary footer, then the committed Summary once every app has synced once
	// — after which incremental syncs just stream. Stop the footer on exit so a
	// Ctrl-C mid-convergence never leaves it pinned.
	reporter := startWatchReporter(os.Stderr, out, log, prog, apps, kubeContext)
	defer reporter.stop()
	return loop.Run(ctx, apps, syncFn, loop.Options{
		Debounce:    *debounce,
		MaxParallel: *maxParallel,
		Render:      renderOpts,
		Build:       makeBuildFunc(ctx, cfg, prog, kubeContext),
		Overrides:   overrides,
		Log:         log,
		Report:      reporter.report,
		// A build/render failure (or a failed sync) ends the run without a Report;
		// remove the live group so a retry starts fresh rather than stacking rows.
		OnError: func(app string, _ error) { prog.finish(app, nil) },
		Gate:    gate,
		// On settling back to idle (convergence done, or a rebuild batch finished),
		// commit a Summary block and log "watching for changes".
		OnIdle: reporter.onIdle,
	})
}

// interactiveTerminal reports whether ksync can run the confirmation picker: it
// needs stdin to read raw keystrokes from and stderr (where it draws) to be a
// terminal. Under a pipe, a redirect, or a process manager this is false and the
// loop falls back to auto-rebuild.
func interactiveTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// buildGate drives the interactive confirmation picker for the watch loop. Its
// Ask launches one picker goroutine — the loop guarantees no overlap by issuing
// exactly one prompt at a time — which reads the user's choice via
// ui.ConfirmBuilds and reports it on Decisions; a quit (Ctrl-C) cancels the
// run instead (in raw mode the terminal sends no SIGINT).
type buildGate struct {
	ctx       context.Context
	cancel    context.CancelFunc
	w         io.Writer // stderr: where the picker draws and skip notes print
	out       ui.Colors
	decisions chan loop.Decision
}

func newBuildGate(ctx context.Context, cancel context.CancelFunc, w io.Writer, out ui.Colors) *loop.Gate {
	g := &buildGate{ctx: ctx, cancel: cancel, w: w, out: out, decisions: make(chan loop.Decision)}
	return &loop.Gate{Ask: g.ask, Decisions: g.decisions}
}

func (g *buildGate) ask(pending []loop.PendingItem) {
	items := make([]ui.PromptItem, len(pending))
	for i, p := range pending {
		if p.Build == loop.DeployOnly {
			items[i] = ui.PromptItem{Icon: ui.IconDeploy, Label: p.Label, Note: "manifests only"}
		} else {
			items[i] = ui.PromptItem{Icon: ui.IconBuild, Label: p.Label, Note: p.App}
		}
	}
	go func() {
		selected, build, quit := ui.ConfirmBuilds(g.ctx, os.Stdin, g.w, g.out, items)
		if quit {
			g.cancel()
			return
		}
		var chosen []loop.PendingItem
		if build {
			chosen = make([]loop.PendingItem, 0, len(selected))
			for _, i := range selected {
				chosen = append(chosen, pending[i])
			}
		} else {
			plural := "s"
			if len(pending) == 1 {
				plural = ""
			}
			ui.WriteLine(g.w, g.out.Dim(fmt.Sprintf("— skipped (%d change%s still pending; edit to re-prompt)", len(pending), plural))+"\n")
		}
		select {
		case g.decisions <- loop.Decision{Selected: chosen}:
		case <-g.ctx.Done():
		}
	}()
}

// watchReporter renders the watch loop's per-app sync lines and frames each
// batch — the initial convergence and every later rebuild — the way a `ksync
// sync` run is framed: for a multi-app watch, a Plan up front, then a committed
// Summary block once the batch settles. The initial convergence additionally
// pins a live Summary footer with the running tally while it runs (it can be
// slow); later rebuilds stream their pipelines and commit a Summary when idle.
// Every batch, multi- or single-app, ends with a "watching for changes" log
// line. A single-app watch skips the Plan/footer/Summary and just streams its
// one ship line, like a one-app sync — but still logs the watching line.
//
// The batch tally (synced/agg/degraded) accumulates as apps report and is read
// (under mu) by both the live footer and onIdle, then reset per batch.
type watchReporter struct {
	w     io.Writer
	out   ui.Colors
	log   logr.Logger
	prog  *progress
	total int
	nameW int
	multi bool // frame with a Plan/footer/Summary (more than one app)

	mu       sync.Mutex
	synced   int            // apps synced in the current batch
	agg      loop.SyncStats // apply stats aggregated over the current batch
	degraded []string       // degraded apps in the current batch
	started  time.Time      // initial-convergence start, for the live footer's elapsed
	footer   *ui.Footer     // live tally; pinned only during the initial convergence
}

// startWatchReporter prints the Plan and pins the live Summary footer for a
// multi-app watch; a single-app watch gets an inert reporter (no Plan/footer).
func startWatchReporter(w io.Writer, out ui.Colors, log logr.Logger, prog *progress, apps []config.App, kubeContext string) *watchReporter {
	r := &watchReporter{w: w, out: out, log: log, prog: prog, total: len(apps), nameW: nameColWidth(apps), multi: len(apps) > 1, started: time.Now()}
	if !r.multi {
		return r
	}
	printPlan(w, out, apps, kubeContext)
	r.footer = ui.StartFooter(w, out, func() []string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return summaryLines(out, r.total, r.synced, r.agg, r.degraded, time.Since(r.started), false)
	})
	return r
}

// report commits one completed app sync (the loop's Report hook): it Finishes
// the app's pipeline — a build app's frozen stage tree or a build-less app's
// ship line — and tallies the sync into the current batch (read back by the
// footer and onIdle). The deploy time on the committed line is the deploy
// stage's own, read from the pipeline, so took is not needed here.
func (r *watchReporter) report(app string, stats loop.SyncStats, _ time.Duration) {
	summary, symbol := applyParts(r.out, stats)
	info := &ui.CommitInfo{Summary: summary, Symbol: symbol, NameW: r.nameW}
	r.mu.Lock()
	r.synced++
	r.agg.Applied += stats.Applied
	r.agg.Pruned += stats.Pruned
	r.agg.Failed += stats.Failed
	r.agg.Degraded += stats.Degraded
	if stats.Degraded > 0 {
		r.degraded = append(r.degraded, app)
	}
	r.mu.Unlock()
	r.prog.finish(app, info)
}

// onIdle closes out a batch (the loop's OnIdle hook): it commits the batch's
// Summary block (multi-app only) and logs a watching-for-changes line, then
// resets the per-batch tally for the next one. The initial convergence also has
// a live footer to tear down first, so its pinned block is replaced by the
// committed Summary. took is the batch's wall-clock, from the loop.
func (r *watchReporter) onIdle(took time.Duration) {
	r.mu.Lock()
	synced, agg, degraded := r.synced, r.agg, r.degraded
	r.synced, r.agg, r.degraded = 0, loop.SyncStats{}, nil
	footer := r.footer
	r.footer = nil
	r.mu.Unlock()

	footer.Stop() // nil-safe; present only for the initial convergence
	if r.multi {
		printSummaryBlock(r.w, summaryLines(r.out, r.total, synced, agg, degraded, took, true))
	}
	r.log.Info("finished, watching for changes")
}

// stop removes the live footer; idempotent, so the deferred call after the loop
// exits is harmless when a batch already stopped it. It matters when Ctrl-C
// arrives mid-convergence — without it the footer stays pinned.
func (r *watchReporter) stop() {
	r.mu.Lock()
	footer := r.footer
	r.mu.Unlock()
	footer.Stop()
}

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

// finish commits the app's pipeline (the frozen committed block printed in place
// of the live group) and clears it so the next run starts fresh. A nil info
// removes the group silently — a failed run whose error surfaces elsewhere.
func (p *progress) finish(app string, info *ui.CommitInfo) {
	p.mu.Lock()
	pipe := p.pipes[app]
	delete(p.pipes, app)
	p.mu.Unlock()
	if pipe != nil {
		if info == nil {
			pipe.Discard()
		} else {
			pipe.Finish(*info)
		}
		return
	}
	// No live group for this app (a report with no prior deploy, as in tests, or a
	// path that opened none): still surface the committed line — without the live
	// pipeline there is no deploy stage to time, so the line omits its duration.
	if info != nil {
		var b strings.Builder
		for _, ln := range info.Above {
			b.WriteString(ln + "\n")
		}
		b.WriteString(ui.DeployLine(p.colors, app, "Deploy", info.Summary, info.Symbol, 0, info.NameW) + "\n")
		ui.WriteLine(p.w, b.String())
	}
}

// makeBuildFunc composes building an image with loading it into the cluster, so
// the same path serves one-shot sync and the watch loop. Each build batch adds a
// Build row (and, when the resulting image is fresh, an Import row) to its app's
// pipeline; the verbose command log is shown only when the command fails. The
// load step is a no-op unless the config sets imageLoad (daemon-shared clusters
// need nothing).
// runCtx governs the load step's lifetime: loads coalesce across apps, so a
// single coalesced invocation carries images from several apps and must outlive
// any one app's build phase — it should abort only when the whole run does (Ctrl-C),
// not when one app's build errors and cancels that app's per-build context. Builds
// keep the per-app context they are passed.
func makeBuildFunc(runCtx context.Context, cfg *config.Config, prog *progress, kubeContext string) loop.BuildFunc {
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
	// (k3d image import and friends are not concurrency-safe) and coalesces images
	// that finish while a load runs into the next invocation (see build.Loader).
	loader := &build.Loader{Command: cfg.ImageLoad.Command, KubeContext: kubeContext}
	return func(ctx context.Context, app string, builds []config.Build) ([]string, error) {
		if len(builds) == 0 {
			return nil, nil
		}
		pipe := prog.pipeline(app)
		// A batch is either one ungrouped entry or all the dirty members of one
		// group; builds[0].Group tells which, since the loop never mixes them. The
		// app heads the group, so an ungrouped label is the build's name (the same
		// name the confirmation prompt uses) and a group's label is the group name
		// plus its member count (how many images this one bake produces).
		label := builds[0].Name
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
				err := loader.Load(runCtx, stage, fresh)
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
// is what destroy prints directly (a synced app commits the richer pipeline
// block via ui.CommitInfo instead).
func summaryBlock(c ui.Colors, app string, results []common.ResourceSyncResult, degraded []string, took time.Duration, nameW int) string {
	var b strings.Builder
	s := syncStats(results)
	s.Degraded = len(degraded)
	for _, line := range failureLines(c, results, degraded) {
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "%s\n", appSyncLine(c, app, s, took, nameW))
	return b.String()
}

// failureLines lists the per-resource notices that print above an app's committed
// block: a ✗ for each resource the sync failed on, and a ⚠ for each that applied
// cleanly but is broken at runtime (CrashLoop, failed Job) — a sync success the
// developer still needs to see. Empty on a clean sync (the common case).
func failureLines(c ui.Colors, results []common.ResourceSyncResult, degraded []string) []string {
	var lines []string
	for _, res := range results {
		if res.Status == common.ResultCodeSyncFailed {
			lines = append(lines, fmt.Sprintf("  %s %s: %s", c.Red("✗"), res.ResourceKey.String(), res.Message))
		}
	}
	for _, line := range degraded {
		lines = append(lines, fmt.Sprintf("  %s %s", c.Yellow("⚠"), c.Dim(line)))
	}
	return lines
}

// printSummary writes one app's summary block above any live block (destroy has
// no pipeline of its own to commit).
func printSummary(w io.Writer, c ui.Colors, app string, results []common.ResourceSyncResult, degraded []string, took time.Duration, nameW int) {
	ui.WriteLine(w, summaryBlock(c, app, results, degraded, took, nameW))
}

// applyParts renders the dim apply summary ("21 applied, 2 pruned, …") and the
// health symbol (✓ applied & healthy · ⚠ applied but a resource is degraded · ✗ a
// sync task failed) shared by the committed deploy row (ui.CommitInfo), the
// deploy-only line, and `ksync destroy`.
func applyParts(c ui.Colors, s loop.SyncStats) (summary, symbol string) {
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
	symbol = c.Green("✓")
	if s.Degraded > 0 {
		symbol = c.Yellow("⚠")
	}
	if s.Failed > 0 {
		symbol = c.Red("✗")
	}
	return c.Dim(strings.Join(parts, ", ")), symbol
}

// appSyncLine renders one app's completed-sync status line via the shared
// ui.DeployLine layout — used by `ksync destroy`, which has no pipeline. It passes
// no kind word: destroy is not a deploy, and its 🚢 icon and summary already say
// what happened. took, when > 0, is appended; a zero duration omits it.
func appSyncLine(c ui.Colors, app string, s loop.SyncStats, took time.Duration, nameW int) string {
	summary, symbol := applyParts(c, s)
	return ui.DeployLine(c, app, "", summary, symbol, took, nameW)
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
