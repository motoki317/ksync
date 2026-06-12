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
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2/textlogger"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/loop"
	"github.com/motoki317/ksync/internal/render"
)

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
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return nil, nil, err
	}
	return cfg, fs.Args(), nil
}

func runRender(args []string) error {
	cfg, names, err := loadConfig(flag.NewFlagSet("render", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	r := render.New(render.Options{})
	for i, app := range apps {
		res, err := r.Render(app.Path)
		if err != nil {
			return fmt.Errorf("app %s: %w", app.Name, err)
		}
		if i > 0 {
			fmt.Println("---")
		}
		if _, err := os.Stdout.Write(res.YAML); err != nil {
			return err
		}
	}
	return nil
}

func runSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	prune := fs.Bool("prune", true, "delete tracked resources missing from the rendered output")
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

	log := textlogger.NewLogger(textlogger.NewConfig())
	eng, err := engine.New(cfg.Context, log)
	if err != nil {
		return err
	}
	defer eng.Close()

	r := render.New(render.Options{})
	for _, app := range apps {
		res, err := r.Render(app.Path)
		if err != nil {
			return fmt.Errorf("app %s: %w", app.Name, err)
		}
		results, err := eng.Sync(ctx, app.Name, res.Objects, engine.SyncOptions{Prune: *prune})
		if err != nil {
			return fmt.Errorf("app %s: sync: %w", app.Name, err)
		}
		printSyncResults(os.Stdout, app.Name, results)
	}
	return nil
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	prune := fs.Bool("prune", true, "delete tracked resources missing from the rendered output")
	debounce := fs.Duration("debounce", 200*time.Millisecond, "quiet period after the last change before re-rendering")
	maxParallel := fs.Int("max-parallel", 4, "how many apps may sync concurrently")
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

	log := textlogger.NewLogger(textlogger.NewConfig())
	eng, err := engine.New(cfg.Context, log)
	if err != nil {
		return err
	}
	defer eng.Close()

	syncFn := func(ctx context.Context, app string, objs []*unstructured.Unstructured) error {
		results, err := eng.Sync(ctx, app, objs, engine.SyncOptions{Prune: *prune})
		if err != nil {
			return err
		}
		printSyncResults(os.Stdout, app, results)
		return nil
	}
	return loop.Run(ctx, apps, syncFn, loop.Options{
		Debounce:    *debounce,
		MaxParallel: *maxParallel,
		Log:         log,
	})
}

func runDestroy(args []string) error {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm deleting every tracked resource of the selected apps")
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

	log := textlogger.NewLogger(textlogger.NewConfig())
	eng, err := engine.New(cfg.Context, log)
	if err != nil {
		return err
	}
	defer eng.Close()

	for _, app := range apps {
		// Destroy is a sync to an empty target set: prune removes everything
		// the tracking label scopes to this app, and nothing else.
		results, err := eng.Sync(ctx, app.Name, nil, engine.SyncOptions{Prune: true})
		if err != nil {
			return fmt.Errorf("app %s: destroy: %w", app.Name, err)
		}
		printSyncResults(os.Stdout, app.Name, results)
	}
	return nil
}

func printSyncResults(w io.Writer, app string, results []common.ResourceSyncResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, res := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", app, res.ResourceKey.String(), res.Status, res.Message)
	}
	tw.Flush()
}
