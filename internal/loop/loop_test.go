package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/config"
)

func TestRun_SyncsAllAppsOnStartThenOnlyChangedOnes(t *testing.T) {
	tmp := t.TempDir()
	writeApp(t, tmp, "app1")
	writeApp(t, tmp, "app2")
	apps := []config.App{
		{Name: "app1", Path: filepath.Join(tmp, "app1")},
		{Name: "app2", Path: filepath.Join(tmp, "app2")},
	}
	rec := &recorder{calls: map[string]int{}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, apps, rec.sync, Options{Debounce: 30 * time.Millisecond})
	}()

	// Startup converges every app once.
	waitFor(t, func() bool { return rec.count("app1") == 1 && rec.count("app2") == 1 })

	// A change under one app re-syncs only that app.
	writeFile(t, filepath.Join(tmp, "app1", "configmap.yaml"), configmapYAML("changed"))
	waitFor(t, func() bool { return rec.count("app1") == 2 })
	if got := rec.count("app2"); got != 1 {
		t.Errorf("app2 synced %d times, want 1 (it was not changed)", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRun_RetriesFailedSyncs(t *testing.T) {
	tmp := t.TempDir()
	writeApp(t, tmp, "app1")
	apps := []config.App{{Name: "app1", Path: filepath.Join(tmp, "app1")}}
	rec := &recorder{calls: map[string]int{}, failFirst: 1}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, apps, rec.sync, Options{RetryBase: 30 * time.Millisecond})
	}()

	// First attempt fails; the loop must retry on its own.
	waitFor(t, func() bool { return rec.count("app1") >= 2 })

	cancel()
	<-done
}

func TestRun_PassesRenderedObjectsToSync(t *testing.T) {
	tmp := t.TempDir()
	writeApp(t, tmp, "app1")
	apps := []config.App{{Name: "app1", Path: filepath.Join(tmp, "app1")}}
	var got []*unstructured.Unstructured
	var mu sync.Mutex
	syncFn := func(_ context.Context, _ string, objs []*unstructured.Unstructured) error {
		mu.Lock()
		defer mu.Unlock()
		got = objs
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, apps, syncFn, Options{}) }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 })
	mu.Lock()
	if got[0].GetKind() != "ConfigMap" || got[0].GetName() != "settings" {
		t.Errorf("synced object = %s/%s, want ConfigMap/settings", got[0].GetKind(), got[0].GetName())
	}
	mu.Unlock()
	cancel()
	<-done
}

type recorder struct {
	mu        sync.Mutex
	calls     map[string]int
	failFirst int // fail this many leading calls per app
}

func (r *recorder) sync(_ context.Context, app string, _ []*unstructured.Unstructured) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[app]++
	if r.calls[app] <= r.failFirst {
		return errors.New("induced failure")
	}
	return nil
}

func (r *recorder) count(app string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[app]
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached within deadline")
}

func writeApp(t *testing.T, base, name string) {
	t.Helper()
	writeFile(t, filepath.Join(base, name, "kustomization.yaml"), "resources:\n  - configmap.yaml\n")
	writeFile(t, filepath.Join(base, name, "configmap.yaml"), configmapYAML("hello"))
}

func configmapYAML(v string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  greeting: " + v + "\n"
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
