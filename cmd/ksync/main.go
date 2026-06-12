// Command ksync is a local-development sync loop for Kubernetes: it watches local
// kustomize directories and, on change, renders, diffs, and applies the affected
// app to a local cluster with ArgoCD-parity sync semantics (helm hooks, sync
// waves, prune, server-side apply, health assessment).
//
// Design decisions live in docs/ADR/.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
)

// The Milestone 1 CLI surface. Commands without an implementation yet are
// stubs; listing them all from day one fixes the command names early.
var subcommands = []struct {
	name, summary string
	run           func(args []string) error
}{
	{"watch", "watch app directories and render/diff/apply on change (the main loop)", nil},
	{"sync", "render and sync the given apps once", nil},
	{"diff", "render and show the diff against live cluster state", nil},
	{"render", "render the given apps to stdout", runRender},
	{"destroy", "delete all tracked resources of the given apps", nil},
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

// loadConfig parses the shared -f flag and loads the config; the remaining
// positional args are returned for the subcommand (usually app names).
func loadConfig(name string, args []string) (*config.Config, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
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
	cfg, names, err := loadConfig("render", args)
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
