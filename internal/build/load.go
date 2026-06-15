package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Loader makes freshly built images visible to a cluster whose image store is
// separate from the local docker daemon — k3d and kind keep their own
// containerd store, a remote cluster needs a registry push. It runs a
// user-supplied command once per build batch with $KSYNC_IMAGES set to the
// newline-separated built refs (and $KSYNC_IMAGE to the first, for the
// single-image case), the same contract as a build Command, so ksync carries no
// per-cluster-type knowledge: the command is `k3d image import …`, `kind load
// docker-image …`, `docker push …`, or anything else the target needs.
//
// One Loader is shared across a session's concurrent builds. Unless Parallel is
// set, Load serializes its calls, because some load commands are NOT safe to run
// in parallel against one cluster: k3d's `image import`, for instance, stages
// every import through a shared per-cluster tools node and a tarball named only
// to the second in a shared volume, then deletes "the tarball(s)" and that tools
// node on cleanup — so two imports overlapping in time clobber each other's
// tarball and silently import nothing while still exiting 0 (observed: 4
// parallel imports, 2 images lost). The zero value is therefore safe-by-default
// (serialized); the command layer opts into concurrency for loaders that allow
// it (registry push, `kind load`). Builds stay parallel either way (they are
// independent); only the load step is gated.
type Loader struct {
	// Command runs via `sh -c` with $KSYNC_IMAGES set to the built refs. Empty
	// means the cluster shares the docker daemon's images (Docker Desktop) and
	// Load is a no-op.
	Command string
	// Parallel lets concurrent Load calls run at once. Leave it false for a
	// command that is not concurrency-safe against one cluster (k3d image
	// import); set it for one that is (registry push).
	Parallel bool
	// Exec defaults to running real processes.
	Exec ExecFunc

	mu sync.Mutex // serializes Load when !Parallel; see the type doc
}

// Load makes refs available to the configured cluster in one invocation — a
// single `k3d image import a b c` instead of one import per image — streaming
// the command's progress to out (defaulting to os.Stderr). It is a no-op when no
// command is configured or refs is empty. Concurrent calls serialize unless
// Parallel is set (see the Loader doc), so a caller may invoke Load from
// parallel build goroutines.
// Each ref is a full content-addressed reference Build returned
// (image:ksync-<hash>). $KSYNC_IMAGES is newline-separated so an unquoted use in
// the command word-splits into one argument per image.
func (l *Loader) Load(ctx context.Context, out io.Writer, refs []string) error {
	if l.Command == "" || len(refs) == 0 {
		return nil
	}
	execFn := l.Exec
	if execFn == nil {
		execFn = runProcess
	}
	if out == nil {
		out = os.Stderr
	}
	// Serialize unless the command is declared concurrency-safe: held across the
	// whole exec so overlapping builds load one at a time (see the Loader doc).
	if !l.Parallel {
		l.mu.Lock()
		defer l.mu.Unlock()
	}
	// Run in ksync's working directory: load commands target the cluster, not a
	// build context, so there is no meaningful directory to enter.
	env := []string{
		"KSYNC_IMAGE=" + refs[0],
		"KSYNC_IMAGES=" + strings.Join(refs, "\n"),
	}
	if err := execFn(ctx, "", env, []string{"sh", "-c", l.Command}, out, out); err != nil {
		return fmt.Errorf("loading %s into the cluster: %w", strings.Join(refs, ", "), err)
	}
	return nil
}
