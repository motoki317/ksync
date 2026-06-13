package build

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestLoader_RunsCommandWithRef(t *testing.T) {
	var calls []call
	exec := func(_ context.Context, dir string, env []string, argv []string, _, _ io.Writer) error {
		calls = append(calls, call{dir: dir, env: env, argv: argv})
		return nil
	}
	l := &Loader{Command: "k3d image import --cluster dev $KSYNC_IMAGE", Exec: exec, Output: io.Discard}

	ref := "ghcr.io/team-a/api-b:ksync-012301230123"
	if err := l.Load(context.Background(), []string{ref}); err != nil {
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
	l := &Loader{Command: "k3d image import --cluster dev $KSYNC_IMAGES", Exec: exec, Output: io.Discard}

	refs := []string{"a:ksync-000000000001", "b:ksync-000000000002", "c:ksync-000000000003"}
	if err := l.Load(context.Background(), refs); err != nil {
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
	if err := l.Load(context.Background(), []string{"img:ksync-deadbeefdead"}); err != nil {
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
	l := &Loader{Command: "false", Exec: exec, Output: io.Discard}
	err := l.Load(context.Background(), []string{"img:ksync-deadbeefdead"})
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
}
