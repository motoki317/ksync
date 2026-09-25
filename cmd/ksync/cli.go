package main

import (
	"fmt"
	"os"
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

// Shared-flag registrars keep identical help and defaults across commands (the help
// drifted when these were hand-written per command). SortFlags is left off on
// each command's flag set, so registration order is display order.

func fileFlag(f *pflag.FlagSet) *string {
	p := new(string)
	f.StringVarP(p, "file", "f", "ksync.yaml", "path to the config file")
	return p
}

func contextFlag(f *pflag.FlagSet) *string {
	p := new(string)
	f.StringVar(p, "context", "", "kubectl context to use, must match allowedContexts\n(optional if allowedContexts has one non-glob entry)")
	return p
}

func profileFlag(f *pflag.FlagSet) func() []string {
	profiles := f.StringSliceP("profile", "p", nil, "also select apps in these profiles, '*' for all\n(repeatable or comma-separated, default "+profileEnv+")")
	return func() []string {
		if f.Changed("profile") {
			return normalizeProfiles(*profiles)
		}
		return parseProfiles(os.Getenv(profileEnv))
	}
}

func maxParallelFlag(f *pflag.FlagSet) *int {
	p := new(int)
	f.IntVar(p, "max-parallel", runtime.NumCPU(), "apps to process at once (0 = no limit)")
	return p
}

func offlineRenderFlag(f *pflag.FlagSet) *bool {
	p := new(bool)
	f.BoolVar(p, "offline-render", false, "render helm charts without the cluster (no lookup)")
	return p
}

func clientDiffFlag(f *pflag.FlagSet) *bool {
	p := new(bool)
	f.BoolVar(p, "client-diff", false, "diff in-process, not by a dry-run apply on the server\n(faster, but re-applies fields the apiserver changes)")
	return p
}

func verboseFlag(f *pflag.FlagSet) *bool {
	p := new(bool)
	f.BoolVarP(p, "verbose", "v", false, "print debug logs")
	return p
}

func pruneFlag(f *pflag.FlagSet) *bool {
	p := new(bool)
	f.BoolVar(p, "prune", true, "delete tracked resources no longer rendered")
	return p
}

func newWatchCmd() *cobra.Command {
	var profiles func() []string
	var (
		path, kctx                             *string
		prune, auto, verbose, offline, clientD *bool
		debounce, timeout                      *time.Duration
		maxParallel                            *int
	)
	images := new(stringSlice)
	cmd := &cobra.Command{
		Use:     "watch [app...]",
		Short:   "sync, then sync again on every change (the main loop)",
		Long:    watchLong,
		Example: watchExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runWatch(path, kctx, prune, auto, verbose, offline, clientD, debounce, timeout, maxParallel, *images, names, profiles())
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	profiles = profileFlag(f)
	prune = pruneFlag(f)
	debounce = f.Duration("debounce", 200*time.Millisecond, "how long files must be quiet before a sync")
	timeout = f.Duration("timeout", defaultSyncTimeout, "time limit for each app's sync (0 = no limit)")
	maxParallel = maxParallelFlag(f)
	f.Var(images, "image", "deploy REF instead of building IMAGE until its source\nchanges (repeatable, or set "+overrideEnv+")")
	auto = f.Bool("auto", false, "act on every change without asking")
	offline = offlineRenderFlag(f)
	clientD = clientDiffFlag(f)
	verbose = verboseFlag(f)
	return cmd
}

func newSyncCmd() *cobra.Command {
	var profiles func() []string
	var (
		path, kctx                              *string
		prune, force, verbose, offline, clientD *bool
		timeout                                 *time.Duration
		maxParallel                             *int
	)
	images := new(stringSlice)
	cmd := &cobra.Command{
		Use:     "sync [app...]",
		Short:   "sync the selected apps once, then exit",
		Long:    syncLong,
		Example: syncExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runSync(path, kctx, prune, force, verbose, offline, clientD, timeout, maxParallel, *images, names, profiles())
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	profiles = profileFlag(f)
	prune = pruneFlag(f)
	force = f.Bool("force", false, "run all hooks again, even with no manifest change")
	timeout = f.Duration("timeout", defaultSyncTimeout, "time limit for each app's sync (0 = no limit)")
	maxParallel = maxParallelFlag(f)
	f.Var(images, "image", "deploy REF instead of building IMAGE\n(repeatable, or set "+overrideEnv+")")
	offline = offlineRenderFlag(f)
	clientD = clientDiffFlag(f)
	verbose = verboseFlag(f)
	return cmd
}

func newDiffCmd() *cobra.Command {
	var profiles func() []string
	var (
		path, kctx           *string
		prune, offline, cliD *bool
		maxParallel          *int
	)
	images := new(stringSlice)
	cmd := &cobra.Command{
		Use:     "diff [app...]",
		Short:   "preview what a sync changes in the cluster",
		Long:    diffLong,
		Example: diffExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runDiff(path, kctx, prune, offline, cliD, maxParallel, *images, names, profiles())
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	profiles = profileFlag(f)
	prune = f.Bool("prune", true, "show the tracked resources that a sync deletes")
	maxParallel = maxParallelFlag(f)
	f.Var(images, "image", "use REF instead of a build of IMAGE\n(repeatable, or set "+overrideEnv+")")
	offline = offlineRenderFlag(f)
	cliD = f.Bool("client-diff", false, "diff in-process, not by a dry-run apply on the server\n(faster, but fields the apiserver changes show as drift)")
	return cmd
}

func newRenderCmd() *cobra.Command {
	var profiles func() []string
	var (
		path, kctx  *string
		offline     *bool
		maxParallel *int
	)
	cmd := &cobra.Command{
		Use:     "render [app...]",
		Short:   "print the rendered manifests of the selected apps",
		Long:    renderLong,
		Example: renderExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runRender(path, kctx, offline, maxParallel, names, profiles())
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	profiles = profileFlag(f)
	maxParallel = maxParallelFlag(f)
	offline = offlineRenderFlag(f)
	return cmd
}

func newImagesCmd() *cobra.Command {
	var profiles func() []string
	var (
		path, kctx    *string
		live, offline *bool
		maxParallel   *int
	)
	cmd := &cobra.Command{
		Use:     "images [app...]",
		Short:   "list the images that the selected apps deploy",
		Long:    imagesLong,
		Example: imagesExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runImages(path, kctx, live, offline, maxParallel, names, profiles())
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	profiles = profileFlag(f)
	live = f.Bool("live", false, "also list the images of pods in the app namespaces")
	maxParallel = maxParallelFlag(f)
	offline = offlineRenderFlag(f)
	return cmd
}

func newDestroyCmd() *cobra.Command {
	var profiles func() []string
	var (
		path, kctx *string
		yes        *bool
		timeout    *time.Duration
	)
	cmd := &cobra.Command{
		Use:     "destroy [app...]",
		Short:   "delete every tracked resource of the selected apps",
		Long:    destroyLong,
		Example: destroyExample,
		GroupID: groupCommands,
		RunE: func(_ *cobra.Command, names []string) error {
			return runDestroy(path, kctx, yes, timeout, names, profiles())
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	path = fileFlag(f)
	kctx = contextFlag(f)
	profiles = profileFlag(f)
	yes = f.Bool("yes", false, "confirm the delete (without it, destroy only prints)")
	timeout = f.Duration("timeout", defaultSyncTimeout, "time limit to delete each app (0 = no limit)")
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
		&cobra.Group{ID: groupTopics, Title: "Concept guides (run 'ksync help <topic>'):"},
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
