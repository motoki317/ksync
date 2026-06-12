// Command ksync is a local-development sync loop for Kubernetes: it watches local
// kustomize directories and, on change, renders, diffs, and applies the affected
// app to a local cluster with ArgoCD-parity sync semantics (helm hooks, sync
// waves, prune, server-side apply, health assessment).
//
// Design decisions live in docs/ADR/.
package main

import (
	"fmt"
	"io"
	"os"
)

// The Milestone 1 CLI surface. Each subcommand is a stub until its slice lands;
// listing them all from day one fixes the command names early.
var subcommands = []struct {
	name, summary string
}{
	{"watch", "watch app directories and render/diff/apply on change (the main loop)"},
	{"sync", "render and sync the given apps once"},
	{"diff", "render and show the diff against live cluster state"},
	{"render", "render the given apps to stdout"},
	{"destroy", "delete all tracked resources of the given apps"},
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
			return fmt.Errorf("%s: not implemented yet", c.name)
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
