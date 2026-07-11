package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/motoki317/ksync/internal/build"
	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/loop"
)

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
	groupParallel := make(map[string]bool, len(cfg.BuildGroups))
	for _, g := range cfg.BuildGroups {
		groupCmd[g.Name] = g.Command
		groupParallel[g.Name] = g.Parallel
	}
	// One GroupGate shared across the parallel per-app builds. It serializes a
	// build group's command across apps (a cold cargo-zigbuild and friends are not
	// concurrency-safe; see build.GroupGate) unless the group sets parallel:true.
	groupGate := &build.GroupGate{}
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
			err = groupGate.Run(group, groupParallel[group], func() error {
				var e error
				refs, e = (&build.Builder{Output: stage, KubeContext: kubeContext}).BuildGroup(ctx, groupCmd[group], builds)
				return e
			})
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
