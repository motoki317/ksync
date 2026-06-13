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
	for _, app := range apps {
		// Build before render so the applied manifests always reference
		// images that exist in the local daemon. Batch by build group so a
		// shared bake/compile runs once (App.BuildBatches over all entries).
		all := make([]int, len(app.Build))
		for i := range all {
			all[i] = i
		}
		images := make([]render.Image, 0, len(app.Build))
		for _, batch := range app.BuildBatches(all) {
			builds := make([]config.Build, len(batch))
			for k, j := range batch {
				builds[k] = app.Build[j]
			}
			refs, err := buildFn(ctx, builds)
			if err != nil {
				return fmt.Errorf("app %s: building %s: %w", app.Name, builds[0].Image, err)
			}
			for k, j := range batch {
				images = append(images, render.Image{Name: app.Build[j].Image, NewTag: build.Tag(refs[k])})
			}
		}
		res, err := r.Render(app.Path)
		if err != nil {
			return fmt.Errorf("app %s: %w", app.Name, err)
		}
		if len(images) > 0 {
			if err := res.SetImages(images); err != nil {
				return fmt.Errorf("app %s: %w", app.Name, err)
			}
		}
		syncCtx, cancel := withTimeout(ctx, *timeout)
		results, err := eng.Sync(syncCtx, app.Name, res.Objects, engine.SyncOptions{Prune: *prune, Namespace: app.Namespace})
		cancel()
		if err != nil {
			return fmt.Errorf("app %s: %w", app.Name, err)
		}
		printSummary(os.Stderr, out, app.Name, results)
	}
	return nil
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
	maxParallel := fs.Int("max-parallel", 4, "how many apps may sync concurrently")
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
	syncFn := func(ctx context.Context, app string, objs []*unstructured.Unstructured) error {
		ctx, cancel := withTimeout(ctx, *timeout)
		defer cancel()
		_, err := eng.Sync(ctx, app, objs, engine.SyncOptions{Prune: *prune, Namespace: nsByApp[app]})
		return err
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
	return func(ctx context.Context, builds []config.Build) ([]string, error) {
		if len(builds) == 0 {
			return nil, nil
		}
		var refs []string
		// A batch is either one ungrouped entry or all the dirty members of one
		// group; builds[0].Group tells which, since the loop never mixes them.
		label := imageName(builds[0].Image)
		if group := builds[0].Group; group != "" {
			label = group
			act := ui.StartActivity(w, colors, "build "+label)
			builder := &build.Builder{Output: act}
			r, err := builder.BuildGroup(ctx, groupCmd[group], builds)
			act.Done(err)
			if err != nil {
				return nil, err
			}
			refs = r
		} else {
			act := ui.StartActivity(w, colors, "build "+label)
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
				act := ui.StartActivity(w, colors, "import "+label)
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
		printSummary(os.Stderr, out, app.Name, results)
	}
	return nil
}

// printSummary condenses one app's sync into a single status line — the count
// of resources applied/pruned, failures called out in red — and lists only the
// resources that failed or ran as hooks. The full per-resource dump is noise on
// a healthy sync (which is the common case); the line is what a developer scans.
func printSummary(w io.Writer, c ui.Colors, app string, results []common.ResourceSyncResult) {
	var b strings.Builder
	var applied, pruned, failed int
	for _, res := range results {
		switch res.Status {
		case common.ResultCodePruned:
			pruned++
		case common.ResultCodeSyncFailed:
			failed++
			fmt.Fprintf(&b, "  %s %s: %s\n", c.Red("✗"), res.ResourceKey.String(), res.Message)
		default:
			applied++
		}
	}

	parts := []string{fmt.Sprintf("%d applied", applied)}
	if pruned > 0 {
		parts = append(parts, fmt.Sprintf("%d pruned", pruned))
	}
	if failed > 0 {
		parts = append(parts, c.Red(fmt.Sprintf("%d failed", failed)))
	}
	symbol := c.Green("✓")
	if failed > 0 {
		symbol = c.Red("✗")
	}
	fmt.Fprintf(&b, "%s %s  %s\n", symbol, c.Bold(app), c.Dim(strings.Join(parts, ", ")))
	_, _ = io.WriteString(w, b.String())
}
