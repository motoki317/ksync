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
// user-supplied command with $KSYNC_IMAGES set to the newline-separated built
// refs (and $KSYNC_IMAGE to the first, for the single-image case), the same
// contract as a build Command, so ksync carries no per-cluster-type knowledge:
// the command is `k3d image import …`, `kind load docker-image …`, `docker push
// …`, or anything else the target needs.
//
// One Loader is shared across a session's concurrent builds. Load is fully
// serialized — never two invocations against one cluster at once — because some
// load commands are not concurrency-safe: k3d's `image import`, for instance,
// stages every import through a shared per-cluster tools node and a tarball
// named only to the second in a shared volume, then deletes "the tarball(s)" and
// that tools node on cleanup, so two overlapping imports clobber each other's
// tarball and silently import nothing while still exiting 0 (observed: 4
// parallel imports, 2 images lost). Serializing is safe for every loader; the
// price is that loads run one wave at a time.
//
// To recover the throughput serializing costs, Load *coalesces*: while one
// invocation runs, refs from other builds that finish in the meantime queue up,
// and the next invocation carries the whole queue in one command — one `k3d
// image import a b c` instead of three. This matters because per-invocation
// fixed cost dominates (a k3d import spins up a tools node and ships a tarball,
// ~seconds, regardless of image count); batching the waiting images amortizes
// that across them. Builds stay parallel either way (they are independent); only
// the load step funnels through here.
type Loader struct {
	// Command runs via `sh -c` with $KSYNC_IMAGES set to the built refs. Empty
	// means the cluster shares the docker daemon's images (Docker Desktop) and
	// Load is a no-op. A command that bulk-loads (`k3d image import $KSYNC_IMAGES`)
	// benefits directly from coalescing; one that cannot take many images at once
	// should loop over $KSYNC_IMAGES itself (`for i in $KSYNC_IMAGES; do …; done`).
	Command string
	// KubeContext is the selected kubectl context, exported to the command as
	// $KSYNC_CONTEXT so one imageLoad can branch per target — a no-op for a
	// daemon-shared cluster, an import/push for a separate-store one. Empty
	// leaves it unset.
	KubeContext string
	// Exec defaults to running real processes.
	Exec ExecFunc

	mu      sync.Mutex     // guards loading + queue
	loading bool           // an invocation is in flight; new calls queue behind it
	queue   []*pendingLoad // refs waiting to be coalesced into the next invocation
}

// pendingLoad is one Load call parked behind a running invocation, waiting to be
// picked up by the next coalesced wave.
type pendingLoad struct {
	refs   []string
	out    io.Writer
	result chan error // the wave's outcome, delivered to this caller
}

// Load makes refs available to the configured cluster, streaming the command's
// progress to out (defaulting to os.Stderr). It is a no-op when no command is
// configured or refs is empty. Calls never overlap; a call that arrives while
// another is in flight blocks until a later wave loads its refs (see the Loader
// doc), so a caller may invoke Load from parallel build goroutines.
//
// A coalesced wave runs under the ctx of whichever caller happens to own the load
// slot, so callers that may coalesce should pass a context with the same lifetime
// (a run-scoped context), not a per-request one — otherwise one caller's
// cancellation could abort another's already-queued load.
//
// Each ref is a full content-addressed reference Build returned
// (image:ksync-<hash>). $KSYNC_IMAGES is newline-separated so an unquoted use in
// the command word-splits into one argument per image.
func (l *Loader) Load(ctx context.Context, out io.Writer, refs []string) error {
	if l.Command == "" || len(refs) == 0 {
		return nil
	}
	l.mu.Lock()
	if l.loading {
		// Park behind the running invocation; a later wave will pick us up and
		// deliver its result here. We do not run anything ourselves.
		p := &pendingLoad{refs: refs, out: out, result: make(chan error, 1)}
		l.queue = append(l.queue, p)
		l.mu.Unlock()
		return <-p.result
	}
	l.loading = true
	l.mu.Unlock()

	// We own the load slot: run our own refs first, then drain everything that
	// queued while we ran, coalescing each wave into a single invocation. We keep
	// the slot until the queue is empty so the waiters never run concurrently.
	err := l.run(ctx, out, refs)
	for {
		l.mu.Lock()
		wave := l.queue
		l.queue = nil
		if len(wave) == 0 {
			l.loading = false
			l.mu.Unlock()
			return err
		}
		l.mu.Unlock()
		waveErr := l.run(ctx, multiOut(wave), batchRefs(wave))
		for _, p := range wave {
			p.result <- waveErr
		}
	}
}

// run executes the command once for refs, mirroring its output to out.
func (l *Loader) run(ctx context.Context, out io.Writer, refs []string) error {
	execFn := l.Exec
	if execFn == nil {
		execFn = runProcess
	}
	if out == nil {
		out = os.Stderr
	}
	// Run in ksync's working directory: load commands target the cluster, not a
	// build context, so there is no meaningful directory to enter.
	env := withContext(l.KubeContext,
		"KSYNC_IMAGE="+refs[0],
		"KSYNC_IMAGES="+strings.Join(refs, "\n"),
	)
	if err := execFn(ctx, "", env, []string{"sh", "-c", l.Command}, out, out); err != nil {
		return fmt.Errorf("loading %s into the cluster: %w", strings.Join(refs, ", "), err)
	}
	return nil
}

// batchRefs flattens a coalesced wave's refs, dropping duplicates so two builds
// that produced the same ref do not load it twice in one command.
func batchRefs(wave []*pendingLoad) []string {
	seen := make(map[string]bool)
	var refs []string
	for _, p := range wave {
		for _, r := range p.refs {
			if !seen[r] {
				seen[r] = true
				refs = append(refs, r)
			}
		}
	}
	return refs
}

// multiOut tees a coalesced wave's command output to every parked caller's
// writer, so each app's import row reflects the shared invocation.
func multiOut(wave []*pendingLoad) io.Writer {
	outs := make([]io.Writer, 0, len(wave))
	for _, p := range wave {
		if p.out != nil {
			outs = append(outs, p.out)
		}
	}
	return io.MultiWriter(outs...)
}
