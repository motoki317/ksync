package build

import (
	"context"
	"fmt"
	"io"
	"os"
)

// Loader makes a freshly built image visible to a cluster whose image store is
// separate from the local docker daemon — k3d and kind keep their own
// containerd store, a remote cluster needs a registry push. It runs a
// user-supplied command once per built ref with $KSYNC_IMAGE set to that ref,
// the same contract as a build Command, so ksync carries no per-cluster-type
// knowledge: the command is `k3d image import …`, `kind load docker-image …`,
// `docker push …`, or anything else the target needs.
type Loader struct {
	// Command runs via `sh -c` with $KSYNC_IMAGE set to the built ref. Empty
	// means the cluster shares the docker daemon's images (Docker Desktop) and
	// Load is a no-op.
	Command string
	// Exec defaults to running real processes.
	Exec ExecFunc
	// Output receives the command's progress; defaults to os.Stderr.
	Output io.Writer
}

// Load makes ref available to the configured cluster. It is a no-op when no
// command is configured. ref is the full content-addressed reference Build
// returned (image:ksync-<hash>).
func (l *Loader) Load(ctx context.Context, ref string) error {
	if l.Command == "" {
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
	env := []string{"KSYNC_IMAGE=" + ref}
	if err := execFn(ctx, "", env, []string{"sh", "-c", l.Command}, out, out); err != nil {
		return fmt.Errorf("loading %s into the cluster: %w", ref, err)
	}
	return nil
}
