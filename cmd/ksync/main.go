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
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"

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

// version is the build version, stamped at release time via
// -ldflags "-X main.version=...". goreleaser and the Nix flake both set it; a
// plain `go build` from source keeps "dev".
var version = "dev"

func main() {
	root := newRootCmd()
	err := root.Execute()
	if msg, code := exitStatus(err, root); code != 0 {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(code)
	}
}

// exitStatus maps a root-command error to the stderr line and process exit code.
// Extracted from main so the error→exit contract is unit-testable without os.Exit.
func exitStatus(err error, root *cobra.Command) (msg string, code int) {
	switch {
	case err == nil:
		return "", 0
	case errors.Is(err, context.Canceled):
		// A Ctrl-C (or a sibling app's failure aborting the run) surfaces as a
		// context cancellation, not a real failure: report it as an interrupt with
		// the conventional 130, never a stack of "timed out" / "context canceled".
		return "ksync: interrupted", 130
	default:
		m := err.Error()
		// Give a genuinely-unknown top-level verb (`ksync frob`) the same next-step
		// nudge SetFlagErrorFunc adds for a bad flag, but pointing at the command
		// list. cobra reports it untyped as `unknown command "frob" for "ksync"`;
		// anchoring on the root path keeps extra args on a real command
		// (`ksync version extra` → `... for "ksync version"`) from getting a hint
		// that points at the wrong fix. Matched by message since cobra exports no
		// sentinel; if the wording changes the hint is simply absent, never wrong.
		if strings.HasPrefix(m, "unknown command ") && strings.HasSuffix(m, `for "`+root.CommandPath()+`"`) {
			m += "\nrun 'ksync help' for the command list"
		}
		return "ksync: " + m, 1
	}
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

func runRender(path, kctx *string, offline *bool, maxParallel *int, names []string) error {
	cfg, err := config.Load(*path)
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
		res, err := r.Render(app.Path, app.ClientRender)
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

func runSync(path, kctx *string, prune, force, verbose, offline, clientDiff *bool, timeout *time.Duration, maxParallel *int, images stringSlice, names []string) error {
	cfg, err := config.Load(*path)
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
		agg.Add(stats)
		if stats.Degraded > 0 {
			degradedApps = append(degradedApps, app.Name)
		}
		mu.Unlock()
		// Commit after tallying so the ✓ line and the footer's bumped count land
		// together (Finish repaints the footer as it writes the block). The deploy
		// time on the committed row is the deploy stage's own (read from the
		// pipeline), so per-app build and apply times both survive the run.
		summary, symbol := applyParts(out, stats)
		summary = withRetryCount(summary, prog.retriesFor(app.Name))
		prog.finish(app.Name, &ui.CommitInfo{Summary: summary, Symbol: symbol, Above: failureLines(out, results, degraded), NameW: nameW})
		return nil
	})
	// Stop and drain any eager builds still running — a failed or aborted run
	// leaves some unfinished — before the Summary, so a late build's failure line
	// cannot land after it. waitBuilds is wg.Wait, safe to call here and again via
	// the defer.
	buildCancel()
	waitBuilds()
	footer.Stop()
	if multi {
		mu.Lock()
		final := summaryLines(out, len(apps), synced, agg, degradedApps, time.Since(started), true)
		mu.Unlock()
		printSummaryBlock(os.Stderr, final)
		printTimings(os.Stderr, out, prog.takeTimings())
	}
	return err
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
	res, err := r.Render(app.Path, app.ClientRender)
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
	results, err := eng.Sync(syncCtx, app.Name, res.Objects, engine.SyncOptions{Prune: prune, Force: force, Namespace: app.Namespace, ServerSide: serverSide, OnWait: deployWait(deploy), OnRetry: deployRetry(deploy, prog.colors, app.Name, prog)})
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

// withTimeout bounds ctx by d, or returns it unchanged (with a no-op cancel)
// when d is non-positive — the documented "no limit" escape hatch.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func runWatch(path, kctx *string, prune, auto, verbose, offline, clientDiff *bool, debounce, timeout *time.Duration, maxParallel *int, images stringSlice, names []string) error {
	cfg, err := config.Load(*path)
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

	// signalContext's cancel doubles as the confirmation picker's quit hook: in
	// raw mode the terminal delivers no SIGINT, so the picker calls cancel itself.
	ctx, cancel := signalContext()
	defer cancel()

	log, engineLog := setupLogging(*verbose)
	// Image overrides seed the loop: an overridden build deploys the supplied ref
	// on first convergence (as `ksync sync` does) instead of building, but its
	// sources are still watched — the first source edit takes over and rebuilds it
	// from then on. See ADR 20260702-watch-image-override-takeover.
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
	syncFn := func(parent context.Context, app string, objs []*unstructured.Unstructured) (loop.SyncStats, error) {
		ctx, cancel := withTimeout(parent, *timeout)
		defer cancel()
		deploy := prog.pipeline(app).Deploy()
		deploy.Start()
		// Reset the retry count before this sync: OnRetry only fires on failure, so
		// a clean re-sync would otherwise inherit the "(N retries)" suffix from an
		// earlier failed sync of the same app whose count was never cleared.
		prog.recordRetries(app, 0)
		results, err := eng.Sync(ctx, app, objs, engine.SyncOptions{Prune: *prune, Namespace: nsByApp[app], ServerSide: !*clientDiff, OnWait: deployWait(deploy), OnRetry: deployRetry(deploy, out, app, prog)})
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
