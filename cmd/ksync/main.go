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
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	renderOpts, cleanup, err := renderOptions(cfg.Context, *offline)
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
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	apps = config.SortByNeeds(apps)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, engineLog := setupLogging(*verbose)
	eng, err := engine.New(cfg.Context, engineLog)
	if err != nil {
		return err
	}
	defer eng.Close()

	out := ui.NewColors(os.Stderr)
	renderOpts, cleanup, err := renderOptions(cfg.Context, *offline)
	if err != nil {
		return err
	}
	defer cleanup()
	r := render.New(renderOpts)
	buildFn := makeBuildFunc(cfg, os.Stderr, out)
	// A whole-stack run gets a plan up front, the summary block pinned live to
	// the bottom (updating as apps finish), and the same block committed on
	// completion; a single-app run already says it all in its one line.
	multi := len(apps) > 1
	if multi {
		printPlan(os.Stderr, out, apps, cfg.Context)
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
		results, degraded, err := syncOneApp(ctx, r, eng, buildFn, app, *prune, *timeout, *maxParallel)
		if err != nil {
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
		// Print after tallying so the ✓ line and the footer's bumped count land
		// together (printSummary repaints the footer as it writes the line).
		printSummary(os.Stderr, out, app.Name, results, degraded)
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
// reference images that exist in the local daemon. The caller prints the
// summary line — after tallying — so the live footer's count tracks the ✓ lines.
func syncOneApp(ctx context.Context, r *render.Renderer, eng *engine.Engine, buildFn loop.BuildFunc, app config.App, prune bool, timeout time.Duration, maxParallel int) ([]common.ResourceSyncResult, []string, error) {
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
	syncCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()
	results, err := eng.Sync(syncCtx, app.Name, res.Objects, engine.SyncOptions{Prune: prune, Namespace: app.Namespace})
	if err != nil {
		return nil, nil, fmt.Errorf("app %s: %w", app.Name, err)
	}
	return results, eng.AppDegraded(app.Name, app.Namespace, res.Objects), nil
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
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log, engineLog := setupLogging(*verbose)
	renderOpts, cleanup, err := renderOptions(cfg.Context, *offline)
	if err != nil {
		return err
	}
	defer cleanup()
	eng, err := engine.New(cfg.Context, engineLog)
	if err != nil {
		return err
	}
	defer eng.Close()

	nsByApp := make(map[string]string, len(apps))
	for _, a := range apps {
		nsByApp[a.Name] = a.Namespace
	}
	// The loop logs build/sync state itself; the per-app timeout keeps one
	// stuck workload from holding a scheduler slot forever — on expiry the
	// sync fails and the scheduler retries it with backoff.
	syncFn := func(ctx context.Context, app string, objs []*unstructured.Unstructured) (loop.SyncStats, error) {
		ctx, cancel := withTimeout(ctx, *timeout)
		defer cancel()
		results, err := eng.Sync(ctx, app, objs, engine.SyncOptions{Prune: *prune, Namespace: nsByApp[app]})
		stats := syncStats(results)
		if err == nil {
			stats.Degraded = len(eng.AppDegraded(app, nsByApp[app], objs))
		}
		return stats, err
	}
	return loop.Run(ctx, apps, syncFn, loop.Options{
		Debounce:    *debounce,
		MaxParallel: *maxParallel,
		Render:      renderOpts,
		Build:       makeBuildFunc(cfg, os.Stderr, ui.NewColors(os.Stderr)),
		Log:         log,
		Resync:      resyncOnEnter(ctx, log),
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

// Stage icons distinguish the kinds of progress/result lines at a glance — a
// build vs an image-load vs an apply all otherwise lead with the same ✓. They
// front each line's label (after the status symbol).
const (
	iconBuild  = "🔨" // docker build / bake
	iconImport = "📦" // imageLoad into the cluster store
	iconSync   = "🚢" // apply / ship to the cluster
)

// makeBuildFunc composes building an image with loading it into the cluster, so
// the same path serves one-shot sync and the watch loop. The load step is a
// no-op unless the config sets imageLoad (daemon-shared clusters need nothing).
// Each external command's output is collapsed into a single live progress line
// (ui.Activity); the verbose build log is shown only when the command fails.
func makeBuildFunc(cfg *config.Config, w io.Writer, colors ui.Colors) loop.BuildFunc {
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
	return func(ctx context.Context, app string, builds []config.Build) ([]string, error) {
		if len(builds) == 0 {
			return nil, nil
		}
		var refs []string
		// A batch is either one ungrouped entry or all the dirty members of one
		// group; builds[0].Group tells which, since the loop never mixes them.
		// A group is built per-app (each app owns a subset of its images), so two
		// apps both build e.g. "rust-services"; the app suffix keeps their
		// progress lines distinct. An ungrouped label is the image name, already
		// unique, so it needs no suffix.
		label := imageName(builds[0].Image)
		if group := builds[0].Group; group != "" {
			label = group + " (" + app + ")"
			act := ui.StartActivity(w, colors, iconBuild+" "+label)
			builder := &build.Builder{Output: act}
			r, err := builder.BuildGroup(ctx, groupCmd[group], builds)
			act.Done(err)
			if err != nil {
				return nil, err
			}
			refs = r
		} else {
			act := ui.StartActivity(w, colors, iconBuild+" "+label)
			builder := &build.Builder{Output: act}
			ref, err := builder.Build(ctx, builds[0])
			act.Done(err)
			if err != nil {
				return nil, err
			}
			refs = []string{ref}
		}
		if cfg.ImageLoad != "" {
			mu.Lock()
			fresh := make([]string, 0, len(refs))
			for _, ref := range refs {
				if !imported[ref] {
					fresh = append(fresh, ref)
				}
			}
			mu.Unlock()
			if len(fresh) > 0 {
				act := ui.StartActivity(w, colors, iconImport+" "+label)
				loader := &build.Loader{Command: cfg.ImageLoad, Output: act}
				err := loader.Load(ctx, fresh)
				act.Done(err)
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
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
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
	eng, err := engine.New(cfg.Context, engineLog)
	if err != nil {
		return err
	}
	defer eng.Close()

	out := ui.NewColors(os.Stderr)
	for _, app := range apps {
		// Destroy is a sync to an empty target set: prune removes everything
		// the tracking label scopes to this app, and nothing else.
		syncCtx, cancel := withTimeout(ctx, *timeout)
		results, err := eng.Sync(syncCtx, app.Name, nil, engine.SyncOptions{Prune: true})
		cancel()
		if err != nil {
			return fmt.Errorf("app %s: destroy: %w", app.Name, err)
		}
		printSummary(os.Stderr, out, app.Name, results, nil)
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

func printSummary(w io.Writer, c ui.Colors, app string, results []common.ResourceSyncResult, degraded []string) {
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
	// took=0: a one-shot sync leaves timing to the Summary block's Duration row.
	fmt.Fprintf(&b, "%s\n", appSyncLine(c, app, s, 0))
	// Through ui.WriteLine so the line erases any in-flight build spinner before
	// printing — apps sync concurrently, so a summary can land mid-spinner.
	ui.WriteLine(w, b.String())
}

// appSyncLine renders one app's completed-sync status line in the ship-emoji
// style shared by `ksync sync` and the watch loop: a health symbol (✓ applied &
// healthy · ⚠ applied but a resource is degraded · ✗ a sync task failed), the
// 🚢 apply icon (distinct from a 🔨 build line), the app name, and a dim summary
// of what changed. took, when > 0, is appended — the watch loop shows per-sync
// timing; one-shot sync leaves it 0.
func appSyncLine(c ui.Colors, app string, s loop.SyncStats, took time.Duration) string {
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
	if took > 0 {
		parts = append(parts, ui.Duration(took))
	}
	symbol := c.Green("✓")
	if s.Degraded > 0 {
		symbol = c.Yellow("⚠")
	}
	if s.Failed > 0 {
		symbol = c.Red("✗")
	}
	return fmt.Sprintf("%s %s %s  %s", symbol, iconSync, c.Bold(app), c.Dim(strings.Join(parts, ", ")))
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
