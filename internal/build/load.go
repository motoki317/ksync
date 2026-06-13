package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// Loader makes freshly built images visible to a cluster whose image store is
// separate from the local docker daemon — k3d and kind keep their own
// containerd store, a remote cluster needs a registry push. It runs a
// user-supplied command once per build batch with $KSYNC_IMAGES set to the
// newline-separated built refs (and $KSYNC_IMAGE to the first, for the
// single-image case), the same contract as a build Command, so ksync carries no
// per-cluster-type knowledge: the command is `k3d image import …`, `kind load
// docker-image …`, `docker push …`, or anything else the target needs.
type Loader struct {
	// Command runs via `sh -c` with $KSYNC_IMAGES set to the built refs. Empty
	// means the cluster shares the docker daemon's images (Docker Desktop) and
	// Load is a no-op.
	Command string
	// Exec defaults to running real processes.
	Exec ExecFunc
	// Output receives the command's progress; defaults to os.Stderr.
	Output io.Writer
}

// Load makes refs available to the configured cluster in one invocation — a
// single `k3d image import a b c` instead of one import per image. It is a
// no-op when no command is configured or refs is empty. Each ref is a full
// content-addressed reference Build returned (image:ksync-<hash>). $KSYNC_IMAGES
// is newline-separated so an unquoted use in the command word-splits into one
// argument per image.
func (l *Loader) Load(ctx context.Context, refs []string) error {
	if l.Command == "" || len(refs) == 0 {
		return nil
	}
	execFn := l.Exec
	if execFn == nil {
		execFn = runProcess
	}
	out := l.Output
	if out == nil {
		out = os.Stderr
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
