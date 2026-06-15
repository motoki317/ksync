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

// With Parallel set (a concurrency-safe loader: registry push, kind load), Load
// must NOT serialize. The fake exec signals entry then blocks until released, so
// all n calls have to be in flight at once; a serializing impl would let only
// one enter and this would time out.
func TestLoader_ParallelRunsConcurrently(t *testing.T) {
	const n = 4
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	exec := func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		entered <- struct{}{}
		<-release
		return nil
	}
	l := &Loader{Command: "docker push $KSYNC_IMAGES", Exec: exec, Parallel: true}

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
	timeout := time.After(2 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-timeout:
			t.Fatalf("only %d/%d loads ran concurrently; Parallel not honored (serialized)", i, n)
		}
	}
	close(release)
	wg.Wait()
}
