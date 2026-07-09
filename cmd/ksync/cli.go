package main

import (
	"fmt"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// This file wires the cobra command tree. The help prose lives in help.go; the
// command bodies (runSync, runWatch, …) live in main.go and the per-command
// files. Flags are pointer-bound so a body reads *flag exactly as it did under
// the stdlib flag package — the framework swap changes parsing and help, not the
// command logic.

// Command groups partition the root help: the runnable commands, and the
// help-only concept pages ('ksync help <topic>').
const (
	groupCommands = "commands"
	groupTopics   = "topics"
)

// Shared-flag registrars. Each adds one flag to f and returns its bound pointer,
// so every command that offers a flag gets identical help and defaults (the help
// drifted when these were hand-written per command). SortFlags is left off on
// each command's flag set, so registration order is display order.

func fileFlag(f *pflag.FlagSet) *string {
	p := new(string)
	f.StringVarP(p, "file", "f", "ksync.yaml", "path to the ksync config file")
	return p
}

func contextFlag(f *pflag.FlagSet) *string {
	p := new(string)
	f.StringVar(p, "context", "", "kubectl context to target; must be listed in allowedContexts (default: the sole allowedContexts entry, when exactly one)")
	return p
}

func maxParallelFlag(f *pflag.FlagSet) *int {
	p := new(int)
	f.IntVar(p, "max-parallel", runtime.NumCPU(), "how many apps to process concurrently (0 = no limit)")
	return p
}

func offlineRenderFlag(f *pflag.FlagSet) *bool {
	p := new(bool)
	f.BoolVar(p, "offline-render", false, "render helm charts without live-cluster lookup (charts using helm lookup will not resolve)")
	return p
}

func clientDiffFlag(f *pflag.FlagSet) *bool {
	p := new(bool)
	f.BoolVar(p, "client-diff", false, "decide the apply set with the in-process (client-side) diff instead of a server-side dry-run apply; faster, but fields the cluster defaults or prunes are re-applied every sync")
	return p
}

func verboseFlag(f *pflag.FlagSet) *bool {
	p := new(bool)
	f.BoolVarP(p, "verbose", "v", false, "also log per-change tracing")
	return p
}

func pruneFlag(f *pflag.FlagSet) *bool {
	p := new(bool)
	f.BoolVar(p, "prune", true, "delete tracked resources missing from the rendered output")
	return p
}

func newWatchCmd() *cobra.Command {
	var (
		path, kctx                             *string
		prune, auto, verbose, offline, clientD *bool
		debounce, timeout                      *time.Duration
		maxParallel                            *int
	)
	images := new(stringSlice)
	cmd := &cobra.Command{
		Use:     "watch [app...]",
		Short:   "watch app directories and sync affected apps on change (the main loop)",
		Long:    watchLong,
		Example: watchExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runWatch(path, kctx, prune, auto, verbose, offline, clientD, debounce, timeout, maxParallel, *images, names)
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	prune = pruneFlag(f)
	debounce = f.Duration("debounce", 200*time.Millisecond, "quiet period after the last change before re-rendering")
	timeout = f.Duration("timeout", defaultSyncTimeout, "max time to wait for one app to converge before retrying (0 = no limit)")
	maxParallel = maxParallelFlag(f)
	f.Var(images, "image", "seed a pre-built image instead of building it, until its source changes: IMAGE=REF (repeatable; also via "+overrideEnv+")")
	auto = f.Bool("auto", false, "rebuild and redeploy automatically on every change, skipping the confirmation prompt")
	offline = offlineRenderFlag(f)
	clientD = clientDiffFlag(f)
	verbose = verboseFlag(f)
	return cmd
}

func newSyncCmd() *cobra.Command {
	var (
		path, kctx                              *string
		prune, force, verbose, offline, clientD *bool
		timeout                                 *time.Duration
		maxParallel                             *int
	)
	images := new(stringSlice)
	cmd := &cobra.Command{
		Use:     "sync [app...]",
		Short:   "render and sync the given apps once, then exit",
		Long:    syncLong,
		Example: syncExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runSync(path, kctx, prune, force, verbose, offline, clientD, timeout, maxParallel, *images, names)
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	prune = pruneFlag(f)
	force = f.Bool("force", false, "re-run hooks even when manifests are unchanged (re-applies PostSync Jobs; a failed hook is retried regardless)")
	timeout = f.Duration("timeout", defaultSyncTimeout, "max time to converge (retrying failures) before giving up (0 = retry until converged or interrupted)")
	maxParallel = maxParallelFlag(f)
	f.Var(images, "image", "deploy a pre-built image instead of building it: IMAGE=REF (repeatable; also via "+overrideEnv+")")
	offline = offlineRenderFlag(f)
	clientD = clientDiffFlag(f)
	verbose = verboseFlag(f)
	return cmd
}

func newDiffCmd() *cobra.Command {
	var (
		path, kctx           *string
		prune, offline, cliD *bool
		maxParallel          *int
	)
	images := new(stringSlice)
	cmd := &cobra.Command{
		Use:     "diff [app...]",
		Short:   "show what a sync would change against live cluster state",
		Long:    diffLong,
		Example: diffExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runDiff(path, kctx, prune, offline, cliD, maxParallel, *images, names)
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	prune = f.Bool("prune", true, "show tracked resources a sync would prune (delete)")
	maxParallel = maxParallelFlag(f)
	f.Var(images, "image", "diff as if this pre-built image were deployed: IMAGE=REF (repeatable; also via "+overrideEnv+")")
	offline = offlineRenderFlag(f)
	cliD = f.Bool("client-diff", false, "diff in-process (client-side) instead of via a server-side dry-run apply; faster, but fields the cluster defaults or prunes can show as drift")
	return cmd
}

func newRenderCmd() *cobra.Command {
	var (
		path, kctx  *string
		offline     *bool
		maxParallel *int
	)
	cmd := &cobra.Command{
		Use:     "render [app...]",
		Short:   "render the given apps' manifests to stdout",
		Long:    renderLong,
		Example: renderExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runRender(path, kctx, offline, maxParallel, names)
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	maxParallel = maxParallelFlag(f)
	offline = offlineRenderFlag(f)
	return cmd
}

func newImagesCmd() *cobra.Command {
	var (
		path, kctx    *string
		live, offline *bool
		maxParallel   *int
	)
	cmd := &cobra.Command{
		Use:     "images [app...]",
		Short:   "list the container images the given apps deploy",
		Long:    imagesLong,
		Example: imagesExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runImages(path, kctx, live, offline, maxParallel, names)
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	live = f.Bool("live", false, "also include images of pods in the apps' namespaces (captures operator-derived images, e.g. ECK Elasticsearch, that rendered manifests never name)")
	maxParallel = maxParallelFlag(f)
	offline = offlineRenderFlag(f)
	return cmd
}

func newDestroyCmd() *cobra.Command {
	var (
		path, kctx *string
		yes        *bool
		timeout    *time.Duration
	)
	cmd := &cobra.Command{
		Use:     "destroy [app...]",
		Short:   "delete all tracked resources of the given apps",
		Long:    destroyLong,
		Example: destroyExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runDestroy(path, kctx, yes, timeout, names)
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	yes = f.Bool("yes", false, "confirm deleting every tracked resource of the selected apps")
	timeout = f.Duration("timeout", defaultSyncTimeout, "max time to wait for one app's resources to delete (0 = no limit)")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "print the ksync version",
		Args:    cobra.NoArgs,
		GroupID: groupCommands,
		RunE:    func(*cobra.Command, []string) error { fmt.Println(version); return nil },
	}
}

// newRootCmd builds a fresh command tree — fresh flag state per call, so tests
// can execute a command without leaking flag values into the next.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ksync",
		Short: "a local-development sync loop for Kubernetes",
		Long:  rootLong,
		// The command bodies print their own errors and set the exit code in main;
		// cobra should neither reprint the error nor dump usage on a RunE failure
		// (a sync timeout is not a usage mistake).
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       version,
	}
	// A flag parse error is a usage mistake, so point the user at the command's help
	// (SilenceUsage suppresses the full usage dump). Set on the root, FlagErrorFunc is
	// inherited by every subcommand and fires regardless of SilenceErrors. It reaches
	// only genuine flag-parse errors: `-force` parses as `--file=orce` (a config error),
	// so that muscle-memory case is not covered here — see ADR known quirks.
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return fmt.Errorf("%w\nrun '%s --help' for usage", err, c.CommandPath())
	})
	// `ksync --version` prints just the version, matching the `version` command.
	root.SetVersionTemplate("{{.Version}}\n")
	// Give --version the -V shorthand so -v stays free for --verbose on the
	// subcommands; cobra would otherwise claim -v for --version. Pre-registering
	// the flag makes cobra's version handling read this one instead of adding its own.
	root.Flags().BoolP("version", "V", false, "print the ksync version")
	root.AddGroup(
		&cobra.Group{ID: groupCommands, Title: "Commands:"},
		&cobra.Group{ID: groupTopics, Title: "Concept guides (run 'ksync <topic>' or 'ksync help <topic>'):"},
	)
	root.AddCommand(
		newWatchCmd(),
		newSyncCmd(),
		newDiffCmd(),
		newRenderCmd(),
		newImagesCmd(),
		newDestroyCmd(),
		newVersionCmd(),
	)
	root.AddCommand(helpTopics()...)
	return root
}
