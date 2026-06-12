package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIgnored(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/repo/apps/api-b/values.yaml", false},
		{"/repo/apps/api-b/kustomization.yaml", false},
		{"/repo/.git/objects/ab/cdef", true},
		{"/repo/apps/api-b/values.yaml~", true},
		{"/repo/apps/api-b/.values.yaml.swp", true},
		{"/repo/apps/api-b/.#values.yaml", true},
		{"/repo/apps/api-b/4913", true}, // vim's write-permission probe
	}
	for _, tt := range tests {
		if got := Ignored(tt.path); got != tt.want {
			t.Errorf("Ignored(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestWatcher_SeesChangesInNestedDirectories(t *testing.T) {
	tmp := t.TempDir()
	nested := filepath.Join(tmp, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	w := newTestWatcher(t, tmp)

	target := filepath.Join(nested, "values.yaml")
	if err := os.WriteFile(target, []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, w, target)
}

func TestWatcher_SeesFilesInDirectoriesCreatedAfterStart(t *testing.T) {
	tmp := t.TempDir()
	w := newTestWatcher(t, tmp)

	// The new directory must be picked up dynamically (fsnotify itself is
	// not recursive), so a file created inside it afterwards is seen.
	newDir := filepath.Join(tmp, "new")
	if err := os.Mkdir(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // allow the watcher to register the new dir
	target := filepath.Join(newDir, "values.yaml")
	if err := os.WriteFile(target, []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, w, target)
}

func TestWatcher_SkipPredicatePrunesDirectories(t *testing.T) {
	tmp := t.TempDir()
	skipped := filepath.Join(tmp, "target")
	kept := filepath.Join(tmp, "src")
	for _, d := range []string{skipped, kept} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w, err := NewWatcher([]Root{{
		Path: tmp,
		Skip: func(dir string) bool { return dir == skipped },
	}})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// A change in the pruned directory must produce no event; one in the
	// kept directory proves the watcher is alive and ordering the check.
	if err := os.WriteFile(filepath.Join(skipped, "artifact"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(kept, "main.rs")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, w, target)
	select {
	case got := <-w.Events:
		if strings.HasPrefix(got, skipped+string(filepath.Separator)) {
			t.Errorf("got event %q from a pruned directory", got)
		}
	default:
	}
}

func TestWatcher_OverlappingRootKeepsSkippedDirWatched(t *testing.T) {
	tmp := t.TempDir()
	inner := filepath.Join(tmp, "manifests")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	// The outer root prunes everything below it, but the inner root (a
	// manifest dir living inside a build context) must stay watched.
	w, err := NewWatcher([]Root{
		{Path: tmp, Skip: func(dir string) bool { return dir != tmp }},
		{Path: inner},
	})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	target := filepath.Join(inner, "kustomization.yaml")
	if err := os.WriteFile(target, []byte("resources: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, w, target)
}

func newTestWatcher(t *testing.T, roots ...string) *Watcher {
	t.Helper()
	rs := make([]Root, len(roots))
	for i, r := range roots {
		rs[i] = Root{Path: r}
	}
	w, err := NewWatcher(rs)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// expectEvent waits until the watcher reports an event for path (other paths
// may be reported around it, e.g. the parent dir).
func expectEvent(t *testing.T, w *Watcher, path string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got, ok := <-w.Events:
			if !ok {
				t.Fatal("watcher closed before the expected event")
			}
			if got == path {
				return
			}
		case <-deadline:
			t.Fatalf("no event for %s within deadline", path)
		}
	}
}
