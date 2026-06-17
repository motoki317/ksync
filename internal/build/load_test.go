package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoader_RunsCommandWithRef(t *testing.T) {
	var calls []call
	exec := func(_ context.Context, dir string, env []string, argv []string, _, _ io.Writer) error {
		calls = append(calls, call{dir: dir, env: env, argv: argv})
		return nil
	}
	l := &Loader{Command: "k3d image import --cluster dev $KSYNC_IMAGE", Exec: exec}

	ref := "ghcr.io/team-a/api-b:ksync-012301230123"
	if err := l.Load(context.Background(), io.Discard, []string{ref}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if want := []string{"sh", "-c", "k3d image import --cluster dev $KSYNC_IMAGE"}; !slices.Equal(c.argv, want) {
		t.Errorf("argv = %v, want %v", c.argv, want)
	}
	if want := "KSYNC_IMAGE=" + ref; !slices.Contains(c.env, want) {
		t.Errorf("env = %v, missing %q", c.env, want)
	}
}

// The selected kubectl context is exported as $KSYNC_CONTEXT so one imageLoad
// command can branch per target (no-op for a shared daemon, import for a
// separate store).
func TestLoader_ExportsKubeContext(t *testing.T) {
	var calls []call
	exec := func(_ context.Context, dir string, env []string, argv []string, _, _ io.Writer) error {
		calls = append(calls, call{dir: dir, env: env, argv: argv})
		return nil
	}
	l := &Loader{Command: "true", KubeContext: "k3d-dev", Exec: exec}
	if err := l.Load(context.Background(), io.Discard, []string{"img:ksync-1"}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "KSYNC_CONTEXT=k3d-dev"; !slices.Contains(calls[0].env, want) {
		t.Errorf("env = %v, missing %q", calls[0].env, want)
	}
}

// A batch of refs loads in one invocation with $KSYNC_IMAGES newline-separated,
// so an unquoted use word-splits into one argument per image.
func TestLoader_BulkSetsImagesEnv(t *testing.T) {
	var calls []call
	exec := func(_ context.Context, dir string, env []string, argv []string, _, _ io.Writer) error {
		calls = append(calls, call{dir: dir, env: env, argv: argv})
		return nil
	}
	l := &Loader{Command: "k3d image import --cluster dev $KSYNC_IMAGES", Exec: exec}

	refs := []string{"a:ksync-000000000001", "b:ksync-000000000002", "c:ksync-000000000003"}
	if err := l.Load(context.Background(), io.Discard, refs); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 (one bulk import)", len(calls))
	}
	if want := "KSYNC_IMAGES=" + strings.Join(refs, "\n"); !slices.Contains(calls[0].env, want) {
		t.Errorf("env = %v, missing %q", calls[0].env, want)
	}
	if want := "KSYNC_IMAGE=" + refs[0]; !slices.Contains(calls[0].env, want) {
		t.Errorf("env = %v, $KSYNC_IMAGE must hold the first ref %q", calls[0].env, want)
	}
}

func TestLoader_EmptyCommandIsNoOp(t *testing.T) {
	called := false
	exec := func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		called = true
		return nil
	}
	l := &Loader{Exec: exec}
	if err := l.Load(context.Background(), io.Discard, []string{"img:ksync-deadbeefdead"}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if called {
		t.Error("empty command should not exec anything")
	}
}

func TestLoader_PropagatesError(t *testing.T) {
	wantErr := errors.New("import boom")
	exec := func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		return wantErr
	}
	l := &Loader{Command: "false", Exec: exec}
	err := l.Load(context.Background(), io.Discard, []string{"img:ksync-deadbeefdead"})
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
}

// A single Loader is shared across the parallel per-app builds, so its Load must
// never run two of the (cluster-mutating, not-concurrency-safe) commands at
// once: k3d image import races on a shared tools node + per-second tarball and
// silently drops images. The fake exec records the high-water mark of
// concurrent invocations; serialized correctly it stays at 1.
func TestLoader_SerializesConcurrentLoads(t *testing.T) {
	var inFlight, maxInFlight int32
	exec := func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		n := atomic.AddInt32(&inFlight, 1)
		for { // CAS the high-water mark up without losing a concurrent bump
			m := atomic.LoadInt32(&maxInFlight)
			if n <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond) // widen the overlap window a racy impl would expose
		atomic.AddInt32(&inFlight, -1)
		return nil
	}
	l := &Loader{Command: "k3d image import $KSYNC_IMAGES", Exec: exec}

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := fmt.Sprintf("img-%d:ksync-%012d", i, i)
			if err := l.Load(context.Background(), io.Discard, []string{ref}); err != nil {
				t.Errorf("Load: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if maxInFlight != 1 {
		t.Errorf("max concurrent loads = %d, want 1 (loads must serialize)", maxInFlight)
	}
}

// Loads that arrive while one is in flight coalesce: the next invocation carries
// every queued ref in a single command, instead of one command per load — what
// recovers the throughput full serialization costs (one k3d import of N images,
// not N imports). White-box: the internal queue is the synchronization point, so
// the test waits for all stragglers to park rather than relying on a fixed sleep.
func TestLoader_CoalescesQueuedLoads(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var waves [][]string // the $KSYNC_IMAGES of each invocation
	exec := func(_ context.Context, _ string, env []string, _ []string, _, _ io.Writer) error {
		mu.Lock()
		first := len(waves) == 0
		waves = append(waves, imagesEnv(env))
		mu.Unlock()
		if first {
			close(started)
			<-release // hold the first load so the others pile up behind it
		}
		return nil
	}
	l := &Loader{Command: "k3d image import $KSYNC_IMAGES", Exec: exec}

	var wg sync.WaitGroup
	load := func(ref string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Load(context.Background(), io.Discard, []string{ref}); err != nil {
				t.Errorf("Load(%s): %v", ref, err)
			}
		}()
	}

	load("a:ksync-000000000001")
	<-started // the leader holds the load slot

	queued := []string{"b:ksync-000000000002", "c:ksync-000000000003", "d:ksync-000000000004"}
	for _, r := range queued {
		load(r)
	}
	for { // wait until all three have parked behind the leader, so they drain as one wave
		l.mu.Lock()
		n := len(l.queue)
		l.mu.Unlock()
		if n == len(queued) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if len(waves) != 2 {
		t.Fatalf("invocations = %d, want 2 (leader + one coalesced wave)", len(waves))
	}
	if want := []string{"a:ksync-000000000001"}; !slices.Equal(waves[0], want) {
		t.Errorf("first invocation refs = %v, want %v", waves[0], want)
	}
	got := slices.Clone(waves[1])
	slices.Sort(got)
	if !slices.Equal(got, queued) {
		t.Errorf("coalesced invocation refs = %v, want %v (all queued, one command)", got, queued)
	}
}

// imagesEnv extracts the newline-separated $KSYNC_IMAGES the Loader set.
func imagesEnv(env []string) []string {
	const prefix = "KSYNC_IMAGES="
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, prefix); ok {
			return strings.Split(v, "\n")
		}
	}
	return nil
}
