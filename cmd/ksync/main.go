// Command ksync is a local-development sync loop for Kubernetes: it watches local
// kustomize directories and, on change, renders, diffs, and applies the affected
// app to a local cluster with ArgoCD-parity sync semantics (helm hooks, sync
// waves, prune, server-side apply, health assessment).
//
// Design decisions live in docs/ADR/.
package main

import (
	"context"
	"errors"
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

// signalContext returns a context canceled on the first SIGINT/SIGTERM, and
// hard-exits the process on the second. signal.NotifyContext alone is unsafe
// here: registering it disables Go's default-terminate, yet several steps a
// command runs are not ctx-aware — the warm-cache LIST in engine.New
// (gitops-engine EnsureSynced, seconds on a large cluster) and kustomize/helm
// render. A SIGINT during one of those is recorded but ignored, and so is every
// SIGINT after it, so the user cannot force-quit until the blocking step returns
// on its own. The second signal therefore os.Exit(130)s unconditionally; the
// blocking windows are all in cooked mode (the build picker handles its own
// raw-mode Ctrl-C and restores the terminal), so a force-quit leaves no terminal
// state broken. The returned cancel doubles as the watch picker's quit hook.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\ninterrupt received; press Ctrl-C again to force quit")
		cancel()
		<-sig
		os.Exit(130)
	}()
	return ctx, cancel
}

type subcommand struct {
	name, summary string
	run           func(args []string) error
}

// subcommands is the CLI surface, in the order `ksync help` lists them. It is
// populated in init rather than as a var initializer to avoid an
// initialization cycle: summaryOf reads this slice, and the command functions
// named here reach summaryOf through the shared flag-parsing helpers.
var subcommands []subcommand

func init() {
	subcommands = []subcommand{
		{"watch", "watch app directories and render/diff/apply on change (the main loop)", runWatch},
		{"sync", "render and sync the given apps once", runSync},
		{"diff", "render and show the diff against live cluster state", runDiff},
		{"render", "render the given apps to stdout", runRender},
		{"images", "list the container images the given apps deploy (canonical refs, for cache scoping)", runImages},
		{"destroy", "delete all tracked resources of the given apps", runDestroy},
		{"version", "print the ksync version", runVersion},
	}
}

// version is the build version, stamped at release time via
// -ldflags "-X main.version=...". goreleaser and the Nix flake both set it; a
// plain `go build` from source keeps "dev".
var version = "dev"

func runVersion([]string) error {
	fmt.Println(version)
	return nil
}

func main() {
	err := run(os.Args[1:])
	switch {
	case err == nil:
		return
	case errors.Is(err, flag.ErrHelp):
		// `-h`/`--help` on a command: usage was already printed to stdout by the
		// flag parse. A help request is success, not an error.
		return
	case errors.Is(err, context.Canceled):
		// A Ctrl-C (or a sibling app's failure aborting the run) surfaces as a
		// context cancellation, not a real failure: report it as an interrupt with
		// the conventional 130, never a stack of "timed out" / "context canceled".
		fmt.Fprintln(os.Stderr, "ksync: interrupted")
		os.Exit(130)
	default:
		fmt.Fprintln(os.Stderr, "ksync:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no command given (run 'ksync help')")
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	case "-V", "--version", "-version":
		return runVersion(nil)
	}
	for _, c := range subcommands {
		if args[0] == c.name {
			return c.run(args[1:])
		}
	}
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q (run 'ksync help')", args[0])
}

func usage(w io.Writer) {
	var b strings.Builder
	b.WriteString("ksync — a local-development sync loop for Kubernetes\n\n")
	b.WriteString("usage: ksync <command> [flags] [app...]\n\n")
	b.WriteString("commands:\n")
	for _, c := range subcommands {
		fmt.Fprintf(&b, "  %-9s %s\n", c.name, c.summary)
	}
	b.WriteString("\nrun 'ksync <command> -h' for a command's flags.\n")
	fmt.Fprint(w, b.String())
}

// summaryOf returns a subcommand's one-line description for its help banner.
func summaryOf(name string) string {
	for _, c := range subcommands {
		if c.name == name {
			return c.summary
		}
	}
	return ""
}

// newSubFlagSet builds a subcommand flag set that prints its own help and parse
// errors exactly once, on streams we choose. flag's auto-output is discarded and
// its Usage suppressed; parseInterspersed prints help to stdout on `-h` and the
// error (with a `-h` hint) to stderr otherwise — so neither is duplicated nor
// sent to the wrong stream.
func newSubFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// printSubUsage writes a command's help: its summary, a usage line, and the flag
// defaults — what `ksync <command> -h` shows.
func printSubUsage(w io.Writer, fs *flag.FlagSet) {
	if s := summaryOf(fs.Name()); s != "" {
		fmt.Fprintf(w, "%s\n\n", s)
	}
	fmt.Fprintf(w, "usage: ksync %s [flags] [app...]\n\nflags:\n", fs.Name())
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
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
// --context override, else the sole allowedContexts entry when the config names
// exactly one concrete context. ksync never reads the kubeconfig current-context
// (see config.SelectContext); an ambiguous allowlist with no --context is refused
// rather than guessed.
func resolveContext(cfg *config.Config, override string) (string, error) {
	kubeContext, err := cfg.SelectContext(override)
	if err != nil {
		return "", err
	}
	// A --context resolved by glob (not an exact allowlist entry) names the
	// cluster it matched, so a per-worktree pattern like k3s-* never acts on a
	// surprise cluster silently.
	if cfg.MatchedViaGlob(kubeContext) {
		fmt.Fprintf(os.Stderr, "ksync: context %q matched a glob in allowedContexts\n", kubeContext)
	}
	return kubeContext, nil
}

// renderContext resolves the context for a render-only command, or "" when the
// render touches no cluster (offline): no context is needed, so an ambiguous
// allowlist must not force a --context that would go unused.
func renderContext(cfg *config.Config, override string, offline bool) (string, error) {
	if offline {
		return "", nil
	}
	return resolveContext(cfg, override)
}

// contextFlag registers the shared --context flag on a subcommand's flag set.
func contextFlag(fs *flag.FlagSet) *string {
	return fs.String("context", "", "kubectl context to target; must be listed in allowedContexts (default: the sole allowedContexts entry, when exactly one)")
}

// maxParallelFlag registers the shared --max-parallel flag. One definition keeps
// the help identical across commands (it drifted before) and lets Go append the
// real default (CPU count) instead of a hand-written, duplicated note. 0 lifts
// the cap (one worker per app), the same in every command.
func maxParallelFlag(fs *flag.FlagSet) *int {
	return fs.Int("max-parallel", runtime.NumCPU(), "how many apps to process concurrently (0 = no limit)")
}

// offlineRenderFlag registers the shared --offline-render flag. "lookup" is not
// backtick-quoted: a backtick would make flag treat it as the value placeholder,
// printing the nonsensical "-offline-render lookup" for a boolean flag.
func offlineRenderFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("offline-render", false, "render helm charts without live-cluster lookup (charts using helm lookup will not resolve)")
}

// clientDiffFlag registers the shared --client-diff flag for the apply paths
// (sync, watch).
func clientDiffFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("client-diff", false, "decide the apply set with the in-process (client-side) diff instead of a server-side dry-run apply; faster, but fields the cluster defaults or prunes are re-applied every sync")
}

// verboseFlag registers the shared -v flag.
func verboseFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("v", false, "verbose: also log per-change tracing")
}

// parseInterspersed parses fs allowing flags and positional args (app names) in
// any order — `sync app -timeout 5m` works as well as `sync -timeout 5m app`.
// Go's flag package stops at the first positional, which surprises developers
// who put the app name first; this permutes by re-parsing past each positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var names []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				printSubUsage(os.Stdout, fs)
				return nil, err
			}
			return nil, fmt.Errorf("%w (run 'ksync %s -h' for usage)", err, fs.Name())
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
	fs := newSubFlagSet("render")
	offline := offlineRenderFlag(fs)
	maxParallel := maxParallelFlag(fs)
	kctx := contextFlag(fs)
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	// Offline rendering touches no cluster, so it needs no context — don't make an
	// ambiguous allowlist force a --context that will go unused.
	kubeContext, err := renderContext(cfg, *kctx, *offline)
	if err != nil {
		return err
	}
	renderOpts, cleanup, err := renderOptions(kubeContext, *offline)
	if err != nil {
		return err
	}
	defer cleanup()
	r := render.New(renderOpts)
	lookup := render.NewVarLookup(cfg.Dir())
	// Render and serialize concurrently — each is independent, and both the
	// per-chart helm dry-runs and the YAML marshal are the per-app cost.
	yamls, err := renderConcurrently(apps, *maxParallel, func(app config.App) ([]byte, error) {
		res, err := r.Render(app.Path)
		if err != nil {
			return nil, fmt.Errorf("app %s: %w", app.Name, err)
		}
		// Apply patches so `render` shows what sync deploys (env-dependent when a
		// patch value uses ${VAR}).
		if err := res.ApplyPatches(app.Patches, lookup); err != nil {
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
	fs := newSubFlagSet("sync")
	prune := fs.Bool("prune", true, "delete tracked resources missing from the rendered output")
	force := fs.Bool("force", false, "re-run hooks even when manifests are unchanged (re-applies PostSync Jobs; a failed hook is retried regardless)")
	timeout := fs.Duration("timeout", defaultSyncTimeout, "max time to wait for one app to converge before failing (0 = no limit)")
	maxParallel := maxParallelFlag(fs)
	verbose := verboseFlag(fs)
	offline := offlineRenderFlag(fs)
	clientDiff := clientDiffFlag(fs)
	var images stringSlice
	fs.Var(&images, "image", "deploy a pre-built image instead of building it: `IMAGE=REF` (repeatable; also via "+overrideEnv+")")
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
	if note := excludedNeedsNote(apps, names); note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	kubeContext, err := resolveContext(cfg, *kctx)
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
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
	lookup := render.NewVarLookup(cfg.Dir())
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
	// signalCtx is the SIGINT/SIGTERM context; runByNeeds derives a child it cancels
	// on the first app error. Inside the closure, ctx is that derived child (cancelled
	// by both a user Ctrl-C and a sibling-abort), so the diagnostic gate reads the
	// signal context directly to tell a real interrupt from a sibling-abort.
	signalCtx := ctx
	userInterrupted := func() bool { return signalCtx.Err() != nil }
	err = runByNeeds(ctx, apps, *maxParallel, func(ctx context.Context, app config.App) error {
		bo := awaitBuild(app.Name)
		if bo.err != nil {
			prog.finish(app.Name, nil)
			return fmt.Errorf("app %s: build failed: %w", app.Name, bo.err)
		}
		results, degraded, err := deployApp(ctx, r, eng, prog, app, bo.tags, overrides, lookup, *prune, *force, !*clientDiff, *timeout, userInterrupted)
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
func deployApp(ctx context.Context, r *render.Renderer, eng *engine.Engine, prog *progress, app config.App, tags map[int]string, overrides map[string]render.Image, lookup func(string) (string, bool), prune, force, serverSide bool, timeout time.Duration, userInterrupted func() bool) ([]common.ResourceSyncResult, []string, error) {
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
	// Patches before image injection: a patch guards the committed manifest shape,
	// and SetImages must win on the dev tag and the Always→IfNotPresent fix.
	if err := res.ApplyPatches(app.Patches, lookup); err != nil {
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
	results, err := eng.Sync(syncCtx, app.Name, res.Objects, engine.SyncOptions{Prune: prune, Force: force, Namespace: app.Namespace, ServerSide: serverSide, OnWait: deployWait(deploy)})
	deploy.Done(err)
	if err != nil {
		// Dump what wedged the sync — on a timeout, and on a real user Ctrl-C too (the
		// usual way to abandon a stuck one-shot sync), gathered on a fresh context
		// since this app's own is now cancelled. A sibling app's failure also cancels
		// this app, but userInterrupted is false then, so an aborted-but-progressing
		// app stays quiet while the failed sibling reports its own error.
		reportSyncDiagnostics(eng, prog.w, prog.colors, app.Name, app.Namespace, res.Objects, err, userInterrupted())
		return nil, nil, fmt.Errorf("app %s: %w", app.Name, err)
	}
	return results, eng.AppDegraded(app.Name, app.Namespace, res.Objects), nil
}

// diagnoseTimeout bounds how long the post-timeout diagnostic gather may take,
// so a wedged cluster cannot turn a sync timeout into a hang on the dump.
const diagnoseTimeout = 15 * time.Second

// reportSyncDiagnostics gathers and prints a debug block for an app whose sync
// ended before it became healthy — the unhealthy resources' events plus related
// pods' container state and logs — so the reader sees what wedged it at a glance.
//
// It runs for a health-gate timeout always, and for a cancellation only when
// userInterrupted — a real Ctrl-C of a one-shot `ksync sync`. A user stops a wedged
// deploy by hitting Ctrl-C, not by waiting out a multi-minute --timeout, and still
// wants to see why it was stuck. A sibling app's failure also cancels this app's
// context, but that is not a user interrupt: the failed sibling reports its own
// error, and dumping every still-progressing app on top of it would be noise, so
// userInterrupted gates those out. The watch loop never sets it — there Ctrl-C is a
// routine quit, not a debugging moment.
//
// A second Ctrl-C force-quits (ADR 20260627-double-signal-force-quit) before this
// bounded gather completes, so the wait is always escapable. Diagnose self-selects
// the unhealthy resources, so an app merely mid-rollout (nothing unhealthy when
// interrupted) prints nothing.
//
// The gather runs on a fresh context.Background() bounded by diagnoseTimeout, never
// the sync's own context (which the timeout or cancellation has just closed —
// reading against it returns only "context canceled"). The block is written to
// scrollback as pipeline output rather than through the single-line logger, which
// cannot carry a multi-line dump.
func reportSyncDiagnostics(eng *engine.Engine, w io.Writer, out ui.Colors, app, namespace string, objs []*unstructured.Unstructured, err error, userInterrupted bool) {
	if !shouldDiagnose(err, userInterrupted) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), diagnoseTimeout)
	defer cancel()
	lines := eng.Diagnose(ctx, app, namespace, objs)
	if len(lines) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString(out.Bold("Diagnostics") + " " + out.Dim("— "+app+" did not become healthy") + "\n")
	for _, ln := range lines {
		b.WriteString(ln + "\n")
	}
	ui.WriteLine(ui.SectionPipeline, w, b.String())
}

// shouldDiagnose reports whether a failed sync warrants a diagnostic dump: a
// health-gate timeout always, and a cancellation only when the user interrupted
// the run (a sibling-abort cancellation is excluded — see reportSyncDiagnostics).
func shouldDiagnose(err error, userInterrupted bool) bool {
	var te *engine.TimeoutError
	if errors.As(err, &te) {
		return true
	}
	return userInterrupted && errors.Is(err, context.Canceled)
}

// deployWait reports the health gate's progress on the app's deploy row: each
// poll that is still waiting updates the not-ready count and names the first few
// resources. An already-healthy sync never calls back, so the row simply
// finishes with its elapsed time.
func deployWait(deploy *ui.Stage) func([]engine.ResourceStatus) {
	return func(pending []engine.ResourceStatus) {
		deploy.SetTail(waitingTail(pending))
	}
}

// waitingTail renders the live health-gate line: the not-ready count plus the
// first few resources by short name (with a "+N" overflow), so the developer
// sees WHAT the deploy is waiting on without the line growing unbounded.
func waitingTail(pending []engine.ResourceStatus) string {
	const show = 3
	tail := fmt.Sprintf("waiting for health  %d not ready", len(pending))
	if len(pending) == 0 {
		return tail
	}
	names := make([]string, 0, show)
	for i, p := range pending {
		if i >= show {
			break
		}
		names = append(names, p.ShortName())
	}
	tail += ": " + strings.Join(names, ", ")
	if len(pending) > show {
		tail += fmt.Sprintf(", +%d", len(pending)-show)
	}
	return tail
}

// excludedNeedsNote warns when a targeted sync (explicit app names) omits apps
// the selected ones declare in needs: those dependencies are not synced here and
// are assumed already deployed. runByNeeds skips an out-of-set need rather than
// waiting on it, so without this note a missing dependency surfaces only as a
// downstream failure. Empty for an untargeted run (no names) or when nothing is
// excluded.
func excludedNeedsNote(selected []config.App, names []string) string {
	if len(names) == 0 {
		return ""
	}
	in := make(map[string]bool, len(selected))
	for _, a := range selected {
		in[a.Name] = true
	}
	var missing []string
	seen := map[string]bool{}
	for _, a := range selected {
		for _, dep := range a.Needs {
			if !in[dep] && !seen[dep] {
				seen[dep] = true
				missing = append(missing, dep)
			}
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("note: not syncing dependencies %s (not selected); assuming they are already deployed", strings.Join(missing, ", "))
}

// appNames returns the apps' names in order, for scope and confirmation lines.
func appNames(apps []config.App) []string {
	names := make([]string, len(apps))
	for i, a := range apps {
		names[i] = a.Name
	}
	return names
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
	fs := newSubFlagSet("watch")
	prune := fs.Bool("prune", true, "delete tracked resources missing from the rendered output")
	debounce := fs.Duration("debounce", 200*time.Millisecond, "quiet period after the last change before re-rendering")
	maxParallel := maxParallelFlag(fs)
	timeout := fs.Duration("timeout", defaultSyncTimeout, "max time to wait for one app to converge before retrying (0 = no limit)")
	auto := fs.Bool("auto", false, "rebuild and redeploy automatically on every change, skipping the confirmation prompt")
	verbose := verboseFlag(fs)
	offline := offlineRenderFlag(fs)
	clientDiff := clientDiffFlag(fs)
	kctx := contextFlag(fs)
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	// watch exists to rebuild the stack from source, so an image override (which
	// deploys a pre-built image instead of building) contradicts its purpose. The
	// --image flag is not offered here; reject the env form too, fail-fast, rather
	// than silently ignore a set value (use `ksync sync` to deploy pre-built images).
	if strings.TrimSpace(os.Getenv(overrideEnv)) != "" {
		return fmt.Errorf("%s is set, but watch does not accept image overrides: it rebuilds images from source — use `ksync sync` to deploy a pre-built image", overrideEnv)
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	kubeContext, err := resolveContext(cfg, *kctx)
	if err != nil {
		return err
	}

	// signalContext's cancel doubles as the confirmation picker's quit hook: in
	// raw mode the terminal delivers no SIGINT, so the picker calls cancel itself.
	ctx, cancel := signalContext()
	defer cancel()

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
	syncFn := func(parent context.Context, app string, objs []*unstructured.Unstructured) (loop.SyncStats, error) {
		ctx, cancel := withTimeout(parent, *timeout)
		defer cancel()
		deploy := prog.pipeline(app).Deploy()
		deploy.Start()
		results, err := eng.Sync(ctx, app, objs, engine.SyncOptions{Prune: *prune, Namespace: nsByApp[app], ServerSide: !*clientDiff, OnWait: deployWait(deploy)})
		deploy.Done(err)
		stats := syncStats(results)
		if err == nil {
			stats.Degraded = len(eng.AppDegraded(app, nsByApp[app], objs))
		} else {
			// On a health-gate timeout, dump what wedged the sync (gathered on its
			// own fresh context, since this app's context is now cancelled). A Ctrl-C
			// here is a routine quit of the watch loop, not a debugging moment, so it
			// gets no dump (userInterrupted=false).
			reportSyncDiagnostics(eng, os.Stderr, out, app, nsByApp[app], objs, err, false)
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
		WorkDir:     cfg.Dir(),
		Build:       makeBuildFunc(ctx, cfg, prog, kubeContext),
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

	mu    sync.Mutex
	abort chan struct{} // per-prompt; closed by Abort to refresh the open picker
}

func newBuildGate(ctx context.Context, cancel context.CancelFunc, w io.Writer, out ui.Colors) *loop.Gate {
	g := &buildGate{ctx: ctx, cancel: cancel, w: w, out: out, decisions: make(chan loop.Decision)}
	return &loop.Gate{Ask: g.ask, Abort: g.abortPrompt, Decisions: g.decisions}
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
	abort := make(chan struct{})
	g.mu.Lock()
	g.abort = abort
	g.mu.Unlock()
	go func() {
		selected, build, quit, aborted := ui.ConfirmBuilds(g.ctx, abort, os.Stdin, g.w, g.out, items)
		if quit {
			g.cancel()
			return
		}
		if aborted {
			// The loop asked to refresh: report Reask so it re-asks with the now-larger
			// pending set, acting on nothing here.
			select {
			case g.decisions <- loop.Decision{Reask: true}:
			case <-g.ctx.Done():
			}
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
			ui.WriteLine(ui.SectionLog, g.w, g.out.Dim(fmt.Sprintf("— skipped (%d change%s still pending; edit to re-prompt)", len(pending), plural))+"\n")
		}
		select {
		case g.decisions <- loop.Decision{Selected: chosen}:
		case <-g.ctx.Done():
		}
	}()
}

// abortPrompt closes the in-flight prompt's abort channel so ConfirmBuilds returns
// aborted; the gate then reports Reask. Idempotent per prompt (the channel is
// cleared once closed), and a no-op when no prompt is open.
func (g *buildGate) abortPrompt() {
	g.mu.Lock()
	ch := g.abort
	g.abort = nil
	g.mu.Unlock()
	if ch != nil {
		close(ch)
	}
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
	// The console sets this log line one blank apart from the Summary (or, for a
	// single-app watch, from the pipeline) above it — see ui.Section.
	r.log.Info("Finished, watching for changes")
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
		ui.WriteLine(ui.SectionPipeline, p.w, b.String())
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
	fs := newSubFlagSet("destroy")
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
		return fmt.Errorf("destroy deletes every tracked resource of: %s — re-run with -yes to confirm", strings.Join(appNames(apps), ", "))
	}
	// Always echo the scope: a bare `destroy -yes` (no app names) deletes every
	// app, so the user must see what is about to go and on which cluster.
	fmt.Fprintf(os.Stderr, "destroying %d app(s) on context %s: %s\n", len(apps), kubeContext, strings.Join(appNames(apps), ", "))
	// Dependents go down before their dependencies.
	apps = config.SortByNeeds(apps)
	slices.Reverse(apps)

	ctx, stop := signalContext()
	defer stop()

	_, engineLog := setupLogging(false)
	eng, err := engine.New(kubeContext, engineLog)
	if err != nil {
		return err
	}
	defer eng.Close()

	out := ui.NewColors(os.Stderr)
	nameW := nameColWidth(apps)
	return destroyApps(ctx, apps, *timeout,
		func(ctx context.Context, app string, opts engine.SyncOptions) ([]common.ResourceSyncResult, error) {
			return eng.Sync(ctx, app, nil, opts)
		},
		func(app string, results []common.ResourceSyncResult, took time.Duration) {
			printSummary(os.Stderr, out, app, results, nil, took, nameW)
		})
}

// destroySyncFunc is the slice of engine.Sync that destroyApps needs — narrowed
// so the destroy flow is testable without a live cluster.
type destroySyncFunc func(ctx context.Context, app string, opts engine.SyncOptions) ([]common.ResourceSyncResult, error)

// destroyApps deletes each app's tracked resources by syncing it to an empty
// target with prune, in the given order (the caller reverses needs order so a
// dependent is removed before what it depends on). AllowEmpty is required: the
// empty target is intentional here and would otherwise trip the empty-render
// guard. It stops at the first failure so a wedged delete is not masked.
func destroyApps(ctx context.Context, apps []config.App, timeout time.Duration, sync destroySyncFunc, report func(app string, results []common.ResourceSyncResult, took time.Duration)) error {
	for _, app := range apps {
		syncCtx, cancel := withTimeout(ctx, timeout)
		start := time.Now()
		results, err := sync(syncCtx, app.Name, engine.SyncOptions{Prune: true, AllowEmpty: true})
		cancel()
		if err != nil {
			return fmt.Errorf("app %s: destroy: %w", app.Name, err)
		}
		report(app.Name, results, time.Since(start))
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
	ui.WriteLine(ui.SectionPipeline, w, summaryBlock(c, app, results, degraded, took, nameW))
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
// target context, and the app names — so the developer sees the scope before the
// per-app lines start streaming. It emits only the block's own lines; the console
// sets it apart from the surrounding sections (the log above, the pipelines below)
// by the Plan section kind.
func printPlan(w io.Writer, c ui.Colors, apps []config.App, kubeContext string) {
	names := make([]string, len(apps))
	for i, a := range apps {
		names[i] = a.Name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", c.Bold("Plan"))
	fmt.Fprintf(&b, "  %s %s %s\n", fmt.Sprintf("%d apps", len(apps)), c.Dim("→"), c.Bold(kubeContext))
	fmt.Fprintf(&b, "  %s\n", c.Dim(strings.Join(names, " ")))
	ui.WriteLine(ui.SectionPlan, w, b.String())
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
	// No leading blank: the console sets the Summary apart from the section above
	// it — the live footer is spaced from the items by the block's own separator
	// (drawBlock), the committed block by its Summary section kind.
	lines := []string{c.Bold("Summary")}
	for _, ln := range rows {
		// Right-align the label (padding added before color-wrapping, so the
		// columns line up regardless of escape codes), value after a 2-space gap.
		lines = append(lines, fmt.Sprintf("  %s  %s", c.Dim(fmt.Sprintf("%*s", width, ln.label)), ln.value))
	}
	return lines
}

// printSummaryBlock commits the final summary block to scrollback as the Summary
// section, so the console sets it one blank line apart from the pipelines above
// and the watching-for-changes log below.
func printSummaryBlock(w io.Writer, lines []string) {
	ui.WriteLine(ui.SectionSummary, w, strings.Join(lines, "\n")+"\n")
}
