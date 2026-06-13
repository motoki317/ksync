package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestRun_ResyncTriggerReSyncsEveryApp(t *testing.T) {
	tmp := t.TempDir()
	writeApp(t, tmp, "app1")
	writeApp(t, tmp, "app2")
	apps := []config.App{
		{Name: "app1", Path: filepath.Join(tmp, "app1")},
		{Name: "app2", Path: filepath.Join(tmp, "app2")},
	}
	rec := &recorder{calls: map[string]int{}}
	resync := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, apps, rec.sync, Options{Debounce: 20 * time.Millisecond, Resync: resync})
	}()

	// Startup syncs each app once; no file changes follow.
	waitFor(t, func() bool { return rec.count("app1") == 1 && rec.count("app2") == 1 })

	// A manual resync re-runs every app without any file change.
	resync <- struct{}{}
	waitFor(t, func() bool { return rec.count("app1") == 2 && rec.count("app2") == 2 })

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

// buildApp writes an app whose deployment references image api-b, plus a
// source dir with a Dockerfile, and returns the two config pieces.
func buildApp(t *testing.T, base string) config.App {
	t.Helper()
	writeFile(t, filepath.Join(base, "app1", "kustomization.yaml"), "resources:\n  - deployment.yaml\n")
	writeFile(t, filepath.Join(base, "app1", "deployment.yaml"), deploymentYAML)
	src := filepath.Join(base, "src")
	writeFile(t, filepath.Join(src, "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(src, "main.go"), "package main\n")
	return config.App{
		Name: "app1",
		Path: filepath.Join(base, "app1"),
		Build: []config.Build{{
			Image:      "api-b",
			Context:    src,
			Dockerfile: filepath.Join(src, "Dockerfile"),
		}},
	}
}

const deploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-b
spec:
  selector:
    matchLabels: {app: api-b}
  template:
    metadata:
      labels: {app: api-b}
    spec:
      containers:
        - name: api
          image: api-b:main
`

type fakeBuilder struct {
	mu     sync.Mutex
	count  int // total images built (sum over batches)
	calls  int // buildFn invocations (batches)
	maxLen int // largest batch seen
	fail   int // fail this many leading images
}

func (f *fakeBuilder) build(_ context.Context, builds []config.Build) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(builds) > f.maxLen {
		f.maxLen = len(builds)
	}
	refs := make([]string, len(builds))
	for i, b := range builds {
		f.count++
		if f.count <= f.fail {
			return nil, errors.New("induced build failure")
		}
		refs[i] = fmt.Sprintf("%s:ksync-%012d", b.Image, f.count)
	}
	return refs, nil
}

func (f *fakeBuilder) builds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

func (f *fakeBuilder) widest() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxLen
}

func (f *fakeBuilder) batches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// objectSink captures the image of the synced Deployment per call.
type objectSink struct {
	mu     sync.Mutex
	images []string
}

func (s *objectSink) sync(_ context.Context, _ string, objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		if obj.GetKind() != "Deployment" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		img, _ := containers[0].(map[string]any)["image"].(string)
		s.mu.Lock()
		s.images = append(s.images, img)
		s.mu.Unlock()
	}
	return nil
}

func (s *objectSink) synced() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.images...)
}

func TestRun_BuildsOnStartupAndInjectsTheTag(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	builder := &fakeBuilder{}
	sink := &objectSink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build})
	}()

	waitFor(t, func() bool { return len(sink.synced()) == 1 })
	if got := sink.synced()[0]; got != "api-b:ksync-000000000001" {
		t.Errorf("synced image = %q, want the injected dev tag", got)
	}

	// A manifest-only change re-syncs with the remembered tag, no rebuild.
	writeFile(t, filepath.Join(tmp, "app1", "deployment.yaml"),
		strings.Replace(deploymentYAML, "name: api", "name: api-renamed", 1))
	waitFor(t, func() bool { return len(sink.synced()) == 2 })
	if got := builder.builds(); got != 1 {
		t.Errorf("builds = %d, want 1 (manifest edits must not rebuild)", got)
	}
	if got := sink.synced()[1]; got != "api-b:ksync-000000000001" {
		t.Errorf("synced image = %q, want the remembered dev tag", got)
	}

	// A source change rebuilds and rolls the tag.
	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edited\n")
	waitFor(t, func() bool { return len(sink.synced()) == 3 })
	if got := builder.builds(); got != 2 {
		t.Errorf("builds = %d, want 2 (source edits rebuild)", got)
	}
	if got := sink.synced()[2]; got != "api-b:ksync-000000000002" {
		t.Errorf("synced image = %q, want the new dev tag", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// groupApp writes an app with two grouped build entries sharing one context,
// and a kustomization deploying both images.
func groupApp(t *testing.T, base string) config.App {
	t.Helper()
	writeFile(t, filepath.Join(base, "app1", "kustomization.yaml"), "resources:\n  - deployment.yaml\n")
	writeFile(t, filepath.Join(base, "app1", "deployment.yaml"), twoImageDeploymentYAML)
	src := filepath.Join(base, "src")
	writeFile(t, filepath.Join(src, "main.go"), "package main\n")
	return config.App{
		Name: "app1",
		Path: filepath.Join(base, "app1"),
		Build: []config.Build{
			{Image: "api-b", Context: src, Group: "g"},
			{Image: "api-c", Context: src, Group: "g"},
		},
	}
}

const twoImageDeploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-b
spec:
  selector:
    matchLabels: {app: api-b}
  template:
    metadata:
      labels: {app: api-b}
    spec:
      containers:
        - name: api
          image: api-b:main
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-c
spec:
  selector:
    matchLabels: {app: api-c}
  template:
    metadata:
      labels: {app: api-c}
    spec:
      containers:
        - name: api
          image: api-c:main
`

func TestRun_BuildGroupBatchesDirtyMembers(t *testing.T) {
	tmp := t.TempDir()
	app := groupApp(t, tmp)
	builder := &fakeBuilder{}
	sink := &objectSink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build})
	}()

	// Startup builds both grouped images in ONE batch (one bulk command), and
	// both tags are injected.
	waitFor(t, func() bool { return len(sink.synced()) == 2 })
	if got := builder.widest(); got != 2 {
		t.Errorf("widest batch = %d, want 2 (both group members built together)", got)
	}
	if imgs := sink.synced(); !slices.Contains(imgs, "api-b:ksync-000000000001") || !slices.Contains(imgs, "api-c:ksync-000000000002") {
		t.Errorf("synced images = %v, want both group tags injected", imgs)
	}

	// A source edit dirties both members; they rebuild together in one batch.
	calls := builder.batches()
	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edited\n")
	waitFor(t, func() bool { return len(sink.synced()) == 4 })
	if got := builder.batches() - calls; got != 1 {
		t.Errorf("batches for the shared edit = %d, want 1 (one bulk rebuild)", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRun_DockerignoredChangeDoesNotRebuild(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	writeFile(t, filepath.Join(tmp, "src", ".dockerignore"), "*.md\n")
	builder := &fakeBuilder{}
	sink := &objectSink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build})
	}()
	waitFor(t, func() bool { return len(sink.synced()) == 1 })

	// The ignored file cannot affect the image; prove no rebuild happened by
	// ordering a manifest-driven sync after it and checking the build count.
	writeFile(t, filepath.Join(tmp, "src", "NOTES.md"), "irrelevant\n")
	writeFile(t, filepath.Join(tmp, "app1", "deployment.yaml"),
		strings.Replace(deploymentYAML, "name: api", "name: api-renamed", 1))
	waitFor(t, func() bool { return len(sink.synced()) == 2 })
	if got := builder.builds(); got != 1 {
		t.Errorf("builds = %d, want 1 (dockerignored changes must not rebuild)", got)
	}

	cancel()
	<-done
}

func TestRun_FailedBuildRetriesAndBlocksSync(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	builder := &fakeBuilder{fail: 1}
	sink := &objectSink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, RetryBase: 30 * time.Millisecond, Build: builder.build})
	}()

	// The first build fails: no sync may happen with an unbuilt image; the
	// retry rebuilds and only then syncs, with the fresh tag injected.
	waitFor(t, func() bool { return len(sink.synced()) == 1 })
	if got := builder.builds(); got != 2 {
		t.Errorf("builds = %d, want 2 (initial failure + retry)", got)
	}
	if got := sink.synced()[0]; got != "api-b:ksync-000000000002" {
		t.Errorf("synced image = %q, want the tag of the successful retry build", got)
	}

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
