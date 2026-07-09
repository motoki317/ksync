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

	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
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

// logCapture records every log message the loop emits, so a test can assert on
// the user-facing lines (the retry announcement) rather than only on behavior.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) write(_, args string) {
	c.mu.Lock()
	c.lines = append(c.lines, args)
	c.mu.Unlock()
}

func (c *logCapture) has(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.lines, func(l string) bool { return strings.Contains(l, sub) })
}

// A failed sync must not be silent: the loop announces the retry with the
// attempt number, the backoff delay, and the error text (kept for grep-ability).
func TestRun_AnnouncesSyncRetryWithAttemptAndDelay(t *testing.T) {
	tmp := t.TempDir()
	writeApp(t, tmp, "shop")
	apps := []config.App{{Name: "shop", Path: filepath.Join(tmp, "shop")}}
	rec := &recorder{calls: map[string]int{}, failFirst: 2}
	cap := &logCapture{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, apps, rec.sync, Options{
			RetryBase: 30 * time.Millisecond,
			Log:       funcr.New(cap.write, funcr.Options{}),
		})
	}()

	// Two failures precede success; each queues a retry the loop must announce,
	// with the attempt counter and the doubling backoff (30ms → 60ms).
	waitFor(t, func() bool {
		return cap.has("shop: sync failed (attempt 1); retrying in 30ms: induced failure") &&
			cap.has("shop: sync failed (attempt 2); retrying in 60ms: induced failure")
	})
	waitFor(t, func() bool { return rec.count("shop") >= 3 })

	cancel()
	<-done
}

func TestRun_PassesRenderedObjectsToSync(t *testing.T) {
	tmp := t.TempDir()
	writeApp(t, tmp, "app1")
	apps := []config.App{{Name: "app1", Path: filepath.Join(tmp, "app1")}}
	var got []*unstructured.Unstructured
	var mu sync.Mutex
	syncFn := func(_ context.Context, _ string, objs []*unstructured.Unstructured) (SyncStats, error) {
		mu.Lock()
		defer mu.Unlock()
		got = objs
		return SyncStats{}, nil
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
	count  int      // total images built (sum over batches)
	calls  int      // buildFn invocations (batches)
	maxLen int      // largest batch seen
	fail   int      // fail this many leading images
	apps   []string // owning app name per invocation, in call order
}

func (f *fakeBuilder) build(_ context.Context, app string, builds []config.Build) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.apps = append(f.apps, app)
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

func (s *objectSink) sync(_ context.Context, _ string, objs []*unstructured.Unstructured) (SyncStats, error) {
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
	return SyncStats{}, nil
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
	// The owning app name reaches the build func — it is what disambiguates a
	// group built per-app in the progress label ("build <group> (<app>)").
	builder.mu.Lock()
	gotApp := builder.apps[0]
	builder.mu.Unlock()
	if gotApp != "app1" {
		t.Errorf("build func got app %q, want %q", gotApp, "app1")
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

// A full resync (the Resync trigger, and the identical dropped-event recovery)
// must rebuild a build app, not merely redeploy it: a missed event may have been
// a source edit, so reusing the last tag would ship stale code. Both callers run
// the same resyncAll closure, so the Resync trigger exercises that path.
func TestRun_ResyncRebuildsBuildApps(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	builder := &fakeBuilder{}
	sink := &objectSink{}
	resync := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build, Resync: resync})
	}()

	// Startup builds once and deploys the dev tag.
	waitFor(t, func() bool { return len(sink.synced()) == 1 })
	if got := builder.builds(); got != 1 {
		t.Fatalf("startup builds = %d, want 1", got)
	}

	// A resync rebuilds and deploys the fresh tag — not a bare redeploy of the
	// remembered one.
	resync <- struct{}{}
	waitFor(t, func() bool { return len(sink.synced()) == 2 })
	if got := builder.builds(); got != 2 {
		t.Errorf("builds after resync = %d, want 2 (a resync must rebuild, not redeploy the stale tag)", got)
	}
	if got := sink.synced()[1]; got != "api-b:ksync-000000000002" {
		t.Errorf("resync deployed %q, want the freshly rebuilt dev tag", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// An overridden build seeds the deploy on first convergence but is still watched:
// the first source change drops the override and the entry rebuilds from then on
// (takeover). This app's only build is overridden, so the test also proves a
// fully-overridden app is registered in the build scheduler at startup — without
// that registration the takeover's MarkDirty is a silent no-op, the rebuild never
// schedules, and the deploy gates on it forever.
func TestRun_OverriddenImageTakesOverOnSourceChange(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	builder := &fakeBuilder{}
	sink := &objectSink{}
	overrides := map[string]render.Image{"api-b": {Name: "api-b", NewTag: "supplied-1"}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build, Overrides: overrides})
	}()

	// Startup deploys the supplied ref and builds nothing (same first-time behavior
	// as `ksync sync`).
	waitFor(t, func() bool { return len(sink.synced()) == 1 })
	if got := sink.synced()[0]; got != "api-b:supplied-1" {
		t.Errorf("startup synced image = %q, want the supplied override", got)
	}
	if got := builder.builds(); got != 0 {
		t.Errorf("startup builds = %d, want 0 (an overridden image is not built)", got)
	}

	// A manifest-only edit redeploys but does not take over: the override still
	// stands, so the supplied ref is re-injected and still nothing builds.
	writeFile(t, filepath.Join(tmp, "app1", "deployment.yaml"),
		strings.Replace(deploymentYAML, "name: api", "name: api-renamed", 1))
	waitFor(t, func() bool { return len(sink.synced()) == 2 })
	if got := sink.synced()[1]; got != "api-b:supplied-1" {
		t.Errorf("after manifest edit synced image = %q, want the override re-injected", got)
	}
	if got := builder.builds(); got != 0 {
		t.Errorf("after manifest edit builds = %d, want 0 (only a source change takes over)", got)
	}

	// A source edit takes over: the entry rebuilds and the deploy injects the built
	// tag from now on. The build only schedules because the fully-overridden app
	// was registered in the build scheduler despite having nothing to build at start.
	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edited\n")
	waitFor(t, func() bool { return builder.builds() == 1 && len(sink.synced()) == 3 })
	if got := sink.synced()[2]; got != "api-b:ksync-000000000001" {
		t.Errorf("after source edit synced image = %q, want the freshly built tag", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// With the manual gate, an overridden build's source change is held for the user's
// decision — the override stands while it waits — and confirming it takes over: the
// override drops and the entry rebuilds, so the deploy switches from the supplied
// ref to the freshly built tag ("prompts first, then takes over").
func TestRun_GateReleaseTakesOverOverride(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	app.Build[0].Name = "api-b" // config.Parse defaults this; set it directly here
	builder := &fakeBuilder{}
	sink := &objectSink{}
	overrides := map[string]render.Image{"api-b": {Name: "api-b", NewTag: "supplied-1"}}
	g := newFakeGate()
	defer g.close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build, Overrides: overrides, Gate: g.gate()})
	}()

	// Startup converges automatically (the gate holds only incremental change),
	// deploying the supplied ref.
	waitFor(t, func() bool { return len(sink.synced()) == 1 })
	if got := sink.synced()[0]; got != "api-b:supplied-1" {
		t.Errorf("startup synced image = %q, want the supplied override", got)
	}

	// A source edit is held for confirmation; the override still stands, so nothing
	// is built or redeployed yet.
	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edited\n")
	waitFor(t, func() bool { return g.askCount() == 1 })
	items := g.lastAsk()
	if len(items) != 1 || items[0].App != "app1" || items[0].Build != 0 || items[0].Label != "api-b" {
		t.Fatalf("ask items = %+v, want one build item (app1, entry 0, api-b)", items)
	}
	if got := builder.builds(); got != 0 {
		t.Fatalf("build ran before confirmation: builds=%d, want 0 (held)", got)
	}

	// Confirming takes over: the override drops, the entry rebuilds, and the deploy
	// injects the built tag instead of the supplied ref.
	g.decided <- Decision{Selected: items}
	waitFor(t, func() bool { return builder.builds() == 1 && len(sink.synced()) == 2 })
	if got := sink.synced()[1]; got != "api-b:ksync-000000000001" {
		t.Errorf("after takeover synced image = %q, want the freshly built tag", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// After a fully-overridden app takes over its entry, a full resync must rebuild
// that entry (a fresh tag, not the stale one) and must not deadlock. This guards
// resyncAll's decision to recompute buildability from the current override state:
// the startup `buildable` snapshot is false for a fully-overridden app, so reusing
// it here would skip the build scheduler and gate the redeploy forever.
func TestRun_ResyncAfterTakeoverRebuilds(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	builder := &fakeBuilder{}
	sink := &objectSink{}
	overrides := map[string]render.Image{"api-b": {Name: "api-b", NewTag: "supplied-1"}}
	resync := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build, Overrides: overrides, Resync: resync})
	}()

	// Startup deploys the supplied ref and builds nothing.
	waitFor(t, func() bool { return len(sink.synced()) == 1 })
	if got := sink.synced()[0]; got != "api-b:supplied-1" {
		t.Fatalf("startup synced image = %q, want the supplied override", got)
	}

	// A source edit takes the (only, overridden) entry over: it rebuilds and the
	// deploy switches to the built tag.
	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edited\n")
	waitFor(t, func() bool { return builder.builds() == 1 && len(sink.synced()) == 2 })
	if got := sink.synced()[1]; got != "api-b:ksync-000000000001" {
		t.Fatalf("after takeover synced image = %q, want the first built tag", got)
	}

	// A resync must now rebuild the taken-over entry (a fresh tag), not redeploy the
	// stale one — and must not hang. Reaching a second build proves resyncAll marked
	// the build scheduler for this once-fully-overridden, now-registered app.
	resync <- struct{}{}
	waitFor(t, func() bool { return builder.builds() == 2 && len(sink.synced()) == 3 })
	if got := sink.synced()[2]; got != "api-b:ksync-000000000002" {
		t.Errorf("after resync synced image = %q, want the freshly rebuilt tag", got)
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

// concBuilder reports the peak number of build calls in flight at once. Each
// call blocks at a barrier until `want` calls are concurrent (proving the
// batches build in parallel) or a short timeout elapses (so a sequential
// regression still returns, having only ever reached a peak of 1).
type concBuilder struct {
	want     int
	mu       sync.Mutex
	inFlight int
	peak     int
	n        int
	gate     chan struct{}
}

func newConcBuilder(want int) *concBuilder {
	return &concBuilder{want: want, gate: make(chan struct{})}
}

func (c *concBuilder) build(ctx context.Context, _ string, builds []config.Build) ([]string, error) {
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > c.peak {
		c.peak = c.inFlight
	}
	if c.inFlight >= c.want {
		select {
		case <-c.gate:
		default:
			close(c.gate)
		}
	}
	c.mu.Unlock()

	select {
	case <-c.gate:
	case <-time.After(time.Second):
	case <-ctx.Done():
	}

	c.mu.Lock()
	c.inFlight--
	c.n++
	n := c.n
	c.mu.Unlock()
	refs := make([]string, len(builds))
	for i, b := range builds {
		refs[i] = fmt.Sprintf("%s:ksync-%012d", b.Image, n)
	}
	return refs, nil
}

func (c *concBuilder) peakConcurrency() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

// multiBuildApp writes an app with n ungrouped build entries (n independent
// batches), each image deployed by its own manifest.
func multiBuildApp(t *testing.T, base string, n int) config.App {
	t.Helper()
	src := filepath.Join(base, "src")
	writeFile(t, filepath.Join(src, "main.go"), "package main\n")
	names := []string{"api-b", "api-c", "api-d", "api-e", "api-f"}
	var resources []string
	var builds []config.Build
	for i := 0; i < n; i++ {
		img := names[i]
		writeFile(t, filepath.Join(base, "app1", img+".yaml"), oneDeploymentYAML(img))
		resources = append(resources, "  - "+img+".yaml")
		builds = append(builds, config.Build{Image: img, Context: src})
	}
	writeFile(t, filepath.Join(base, "app1", "kustomization.yaml"), "resources:\n"+strings.Join(resources, "\n")+"\n")
	return config.App{Name: "app1", Path: filepath.Join(base, "app1"), Build: builds}
}

func oneDeploymentYAML(img string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
spec:
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      containers:
        - name: api
          image: %[1]s:main
`, img)
}

func TestRun_BuildsIndependentBatchesConcurrently(t *testing.T) {
	tmp := t.TempDir()
	app := multiBuildApp(t, tmp, 3)
	builder := newConcBuilder(3)
	sink := &objectSink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, MaxParallel: 4, Build: builder.build})
	}()

	// All three batches must be building at once; sequential builds would peak
	// at one. (The barrier releases as soon as the third call arrives.)
	waitFor(t, func() bool { return len(sink.synced()) == 3 })
	if got := builder.peakConcurrency(); got < 3 {
		t.Errorf("peak concurrent builds = %d, want 3 (independent batches must build in parallel)", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestRun_BuildConcurrencyRespectsMaxParallel proves the per-app cap holds: with
// MaxParallel=2 and three batches, at most two build at once.
func TestRun_BuildConcurrencyRespectsMaxParallel(t *testing.T) {
	tmp := t.TempDir()
	app := multiBuildApp(t, tmp, 3)
	// want=2: the barrier releases at two concurrent builds, so the third runs
	// after one frees a slot — peak must never exceed the cap.
	builder := newConcBuilder(2)
	sink := &objectSink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, MaxParallel: 2, Build: builder.build})
	}()

	waitFor(t, func() bool { return len(sink.synced()) == 3 })
	if got := builder.peakConcurrency(); got > 2 {
		t.Errorf("peak concurrent builds = %d, want <= 2 (MaxParallel cap)", got)
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

// buildAppNamed writes a build-app with its own name, image, source dir, and a
// kustomization deploying that one image.
func buildAppNamed(t *testing.T, base, name, img string) config.App {
	t.Helper()
	appDir := filepath.Join(base, name)
	writeFile(t, filepath.Join(appDir, "kustomization.yaml"), "resources:\n  - deployment.yaml\n")
	writeFile(t, filepath.Join(appDir, "deployment.yaml"), oneDeploymentYAML(img))
	src := filepath.Join(base, name+"-src")
	writeFile(t, filepath.Join(src, "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(src, "main.go"), "package main\n")
	return config.App{
		Name:  name,
		Path:  appDir,
		Build: []config.Build{{Image: img, Context: src, Dockerfile: filepath.Join(src, "Dockerfile")}},
	}
}

// TestRun_DependentBuildsWhileDependencyDeploys is the core of the build/deploy
// split: a dependent's build must run while its dependency is still deploying,
// not behind it. App b needs a; a's deploy is held in flight until b's build is
// observed to start. Builds are needs-free, so b builds immediately and the gate
// releases; under the old fused run b's build would wait for a's deploy and this
// would deadlock (caught by the timeout). b's *deploy* still waits for a.
func TestRun_DependentBuildsWhileDependencyDeploys(t *testing.T) {
	tmp := t.TempDir()
	a := buildAppNamed(t, tmp, "a", "img-a")
	b := buildAppNamed(t, tmp, "b", "img-b")
	b.Needs = []string{"a"}

	var once sync.Once
	bBuildStarted := make(chan struct{})
	releaseADeploy := make(chan struct{})
	builder := func(_ context.Context, app string, builds []config.Build) ([]string, error) {
		if app == "b" {
			once.Do(func() { close(bBuildStarted) })
		}
		refs := make([]string, len(builds))
		for i, bld := range builds {
			refs[i] = bld.Image + ":ksync-000000000001"
		}
		return refs, nil
	}

	var mu sync.Mutex
	deployed := map[string]int{}
	syncFn := func(_ context.Context, app string, _ []*unstructured.Unstructured) (SyncStats, error) {
		if app == "a" {
			<-releaseADeploy // hold a's deploy in flight
		}
		mu.Lock()
		deployed[app]++
		mu.Unlock()
		return SyncStats{}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{a, b}, syncFn, Options{Debounce: 10 * time.Millisecond, Build: builder})
	}()

	select {
	case <-bBuildStarted:
	case <-time.After(3 * time.Second):
		close(releaseADeploy) // unblock so Run can shut down cleanly
		cancel()
		<-done
		t.Fatal("b's build did not start while a's deploy was in flight — builds are still gated by needs")
	}
	// a's deploy is still blocked here; releasing it lets the stack converge, with
	// b's deploy following a's (the needs edge on the deploy phase still holds).
	close(releaseADeploy)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return deployed["a"] == 1 && deployed["b"] == 1 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// fakeGate is a test double for the manual gate: it records each Ask's items and
// lets the test drive the Decision channel, so the gate's hold/release behavior
// is exercised without a real terminal.
type fakeGate struct {
	mu      sync.Mutex
	asks    [][]PendingItem
	aborts  int
	decided chan Decision
	done    chan struct{} // closed on cleanup; releases a blocked abort send
}

func newFakeGate() *fakeGate {
	return &fakeGate{decided: make(chan Decision), done: make(chan struct{})}
}

func (g *fakeGate) gate() *Gate { return &Gate{Ask: g.ask, Abort: g.abort, Decisions: g.decided} }

func (g *fakeGate) ask(items []PendingItem) {
	g.mu.Lock()
	g.asks = append(g.asks, items)
	g.mu.Unlock()
}

// abort mimics the real gate: an aborted picker reports Reask, asynchronously since
// the loop calls Abort from its own goroutine and reads decided in its select.
func (g *fakeGate) abort() {
	g.mu.Lock()
	g.aborts++
	g.mu.Unlock()
	go func() {
		select {
		case g.decided <- Decision{Reask: true}:
		case <-g.done:
		}
	}()
}

func (g *fakeGate) abortCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.aborts
}

func (g *fakeGate) close() { close(g.done) }

func (g *fakeGate) askCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.asks)
}

func (g *fakeGate) lastAsk() []PendingItem {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.asks) == 0 {
		return nil
	}
	return append([]PendingItem(nil), g.asks[len(g.asks)-1]...)
}

// A held source change is not built until the user decides; the gate is asked
// with the dirty image, and the matching decision then builds and deploys it.
func TestRun_GateHoldsBuildUntilDecision(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	app.Build[0].Name = "api-b" // config.Parse defaults this; set it directly here
	builder := &fakeBuilder{}
	sink := &objectSink{}
	g := newFakeGate()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build, Gate: g.gate()})
	}()

	// Startup still converges automatically (the gate only holds incremental change).
	waitFor(t, func() bool { return len(sink.synced()) == 1 && builder.builds() == 1 })

	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edited\n")
	waitFor(t, func() bool { return g.askCount() == 1 })
	items := g.lastAsk()
	if len(items) != 1 || items[0].App != "app1" || items[0].Build != 0 || items[0].Label != "api-b" {
		t.Fatalf("ask items = %+v, want one build item (app1, entry 0, api-b)", items)
	}
	if got := builder.builds(); got != 1 {
		t.Fatalf("build ran before the decision: builds=%d, want 1 (held)", got)
	}

	g.decided <- Decision{Selected: items}
	waitFor(t, func() bool { return builder.builds() == 2 && len(sink.synced()) == 2 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// Skipping (an empty decision) builds nothing; a later, new change re-asks.
func TestRun_GateSkipBuildsNothing(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	builder := &fakeBuilder{}
	sink := &objectSink{}
	g := newFakeGate()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build, Gate: g.gate()})
	}()
	waitFor(t, func() bool { return builder.builds() == 1 })

	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edit 1\n")
	waitFor(t, func() bool { return g.askCount() == 1 })
	g.decided <- Decision{} // skip

	// Nothing should build; a skip holds without acting.
	time.Sleep(100 * time.Millisecond)
	if got := builder.builds(); got != 1 {
		t.Fatalf("skip built anyway: builds=%d, want 1", got)
	}

	// A fresh change re-arms the prompt.
	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edit 2\n")
	waitFor(t, func() bool { return g.askCount() == 2 })

	cancel()
	<-done
}

// A manifest-only change is offered as a deploy-only item and, when chosen,
// redeploys without rebuilding.
func TestRun_GateManifestChangeIsDeployOnly(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	builder := &fakeBuilder{}
	sink := &objectSink{}
	g := newFakeGate()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build, Gate: g.gate()})
	}()
	waitFor(t, func() bool { return len(sink.synced()) == 1 && builder.builds() == 1 })

	writeFile(t, filepath.Join(tmp, "app1", "deployment.yaml"),
		strings.Replace(deploymentYAML, "name: api", "name: api-renamed", 1))
	waitFor(t, func() bool { return g.askCount() == 1 })
	items := g.lastAsk()
	if len(items) != 1 || items[0].Build != DeployOnly || items[0].App != "app1" {
		t.Fatalf("ask items = %+v, want one deploy-only item for app1", items)
	}

	g.decided <- Decision{Selected: items}
	waitFor(t, func() bool { return len(sink.synced()) == 2 })
	if got := builder.builds(); got != 1 {
		t.Errorf("manifest-only change rebuilt: builds=%d, want 1", got)
	}

	cancel()
	<-done
}

// Selecting a subset builds only the chosen images; the unselected one stays
// pending and is re-offered once the chosen work finishes.
func TestRun_GateSubsetLeavesRemainderPending(t *testing.T) {
	tmp := t.TempDir()
	app := multiBuildApp(t, tmp, 2) // images api-b, api-c from one shared source dir
	builder := &fakeBuilder{}
	sink := &objectSink{}
	g := newFakeGate()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, MaxParallel: 4, Build: builder.build, Gate: g.gate()})
	}()
	// Startup builds both images.
	waitFor(t, func() bool { return builder.builds() == 2 })

	// A shared-source edit dirties both; the gate offers both.
	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edited\n")
	waitFor(t, func() bool { return g.askCount() == 1 })
	items := g.lastAsk()
	if len(items) != 2 {
		t.Fatalf("ask items = %+v, want both dirty images", items)
	}

	// Choose only the first image.
	g.decided <- Decision{Selected: items[:1]}
	// Exactly one more build runs (the chosen image); the other stays held.
	waitFor(t, func() bool { return builder.builds() == 3 })
	// The remainder is re-offered once the chosen work finishes.
	waitFor(t, func() bool { return g.askCount() == 2 })
	remainder := g.lastAsk()
	if len(remainder) != 1 || remainder[0].App != items[1].App || remainder[0].Build != items[1].Build {
		t.Fatalf("re-offer = %+v, want only the unselected image %+v", remainder, items[1])
	}
	if got := builder.builds(); got != 3 {
		t.Errorf("the unselected image built without being chosen: builds=%d, want 3", got)
	}

	cancel()
	<-done
}

// A change that lands while a prompt is already open must fold into it: the loop
// aborts the stale prompt and re-asks with the new item included, so the developer
// sees every pending app rather than only those dirty when the prompt first opened.
// Regression: the second app's edit was silently held until the first was rebuilt.
func TestRun_GateFoldsNewChangeIntoOpenPrompt(t *testing.T) {
	tmp := t.TempDir()
	appA := buildApp(t, tmp)
	appA.Build[0].Name = "api-b"
	// A second, independent build app with its own source dir, so each app is
	// edited separately (multiBuildApp shares one source, dirtying every image).
	srcB := filepath.Join(tmp, "srcB")
	writeFile(t, filepath.Join(srcB, "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(srcB, "main.go"), "package main\n")
	writeFile(t, filepath.Join(tmp, "app2", "kustomization.yaml"), "resources:\n  - api-c.yaml\n")
	writeFile(t, filepath.Join(tmp, "app2", "api-c.yaml"), oneDeploymentYAML("api-c"))
	appB := config.App{
		Name: "app2",
		Path: filepath.Join(tmp, "app2"),
		Build: []config.Build{{
			Image: "api-c", Name: "api-c", Context: srcB, Dockerfile: filepath.Join(srcB, "Dockerfile"),
		}},
	}

	builder := &fakeBuilder{}
	sink := &objectSink{}
	g := newFakeGate()
	defer g.close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{appA, appB}, sink.sync, Options{Debounce: 20 * time.Millisecond, MaxParallel: 4, Build: builder.build, Gate: g.gate()})
	}()
	// Startup converges both apps.
	waitFor(t, func() bool { return builder.builds() == 2 && len(sink.synced()) == 2 })

	// Edit app A's source → the gate opens, asking for app1 only.
	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // A\n")
	waitFor(t, func() bool { return g.askCount() == 1 })
	if items := g.lastAsk(); len(items) != 1 || items[0].App != "app1" {
		t.Fatalf("first ask = %+v, want one item for app1", items)
	}

	// While that prompt is open, edit app B's source. The loop must abort the open
	// prompt and re-ask with BOTH apps folded in.
	writeFile(t, filepath.Join(srcB, "main.go"), "package main // B\n")
	waitFor(t, func() bool { return len(g.lastAsk()) == 2 })
	if g.abortCount() == 0 {
		t.Errorf("the open prompt should have been aborted to refresh it")
	}
	apps := map[string]bool{}
	for _, it := range g.lastAsk() {
		apps[it.App] = true
	}
	if !apps["app1"] || !apps["app2"] {
		t.Fatalf("re-ask = %+v, want both app1 and app2", g.lastAsk())
	}
	// Nothing built for the held changes yet — still awaiting a decision.
	if got := builder.builds(); got != 2 {
		t.Fatalf("a held change built before any decision: builds=%d, want 2", got)
	}

	// Decide on the full set → both rebuild.
	g.decided <- Decision{Selected: g.lastAsk()}
	waitFor(t, func() bool { return builder.builds() == 4 })

	cancel()
	<-done
}

// idleRecorder counts the loop's OnIdle calls (one per settled batch) and keeps
// the last reported batch duration.
type idleRecorder struct {
	mu    sync.Mutex
	calls int
	last  time.Duration
}

func (r *idleRecorder) onIdle(took time.Duration) {
	r.mu.Lock()
	r.calls++
	r.last = took
	r.mu.Unlock()
}

func (r *idleRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *idleRecorder) lastTook() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// OnIdle fires exactly once when a batch settles back to idle: once for the
// initial convergence (however many apps it synced), and once more per later
// change batch — the signal the command layer turns into a per-batch summary.
func TestRun_OnIdleFiresOncePerBatch(t *testing.T) {
	tmp := t.TempDir()
	writeApp(t, tmp, "app1")
	writeApp(t, tmp, "app2")
	apps := []config.App{
		{Name: "app1", Path: filepath.Join(tmp, "app1")},
		{Name: "app2", Path: filepath.Join(tmp, "app2")},
	}
	rec := &recorder{calls: map[string]int{}}
	idle := &idleRecorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, apps, rec.sync, Options{Debounce: 20 * time.Millisecond, OnIdle: idle.onIdle})
	}()

	// The whole startup convergence settles to idle once, not once per app.
	waitFor(t, func() bool { return rec.count("app1") == 1 && rec.count("app2") == 1 })
	waitFor(t, func() bool { return idle.count() == 1 })
	if idle.lastTook() <= 0 {
		t.Errorf("OnIdle took = %v, want a positive batch duration", idle.lastTook())
	}

	// One change forms a new batch; settling again fires OnIdle a second time.
	writeFile(t, filepath.Join(tmp, "app1", "configmap.yaml"), configmapYAML("changed"))
	waitFor(t, func() bool { return idle.count() == 2 })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A change merely held by the manual gate (then skipped) runs no build or
// deploy, so the loop never re-enters a busy span and OnIdle must not fire — a
// "finished" summary for work that never ran would be misleading.
func TestRun_OnIdleNotFiredByHeldGateChange(t *testing.T) {
	tmp := t.TempDir()
	app := buildApp(t, tmp)
	builder := &fakeBuilder{}
	sink := &objectSink{}
	g := newFakeGate()
	idle := &idleRecorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []config.App{app}, sink.sync, Options{Debounce: 20 * time.Millisecond, Build: builder.build, Gate: g.gate(), OnIdle: idle.onIdle})
	}()
	// Startup convergence still fires OnIdle once (it runs work directly).
	waitFor(t, func() bool { return len(sink.synced()) == 1 })
	waitFor(t, func() bool { return idle.count() == 1 })

	writeFile(t, filepath.Join(tmp, "src", "main.go"), "package main // edited\n")
	waitFor(t, func() bool { return g.askCount() == 1 })
	g.decided <- Decision{} // skip: hold the change, run nothing

	time.Sleep(100 * time.Millisecond)
	if got := idle.count(); got != 1 {
		t.Errorf("a held/skipped change must not fire OnIdle: count=%d, want 1", got)
	}

	cancel()
	<-done
}

type recorder struct {
	mu        sync.Mutex
	calls     map[string]int
	failFirst int // fail this many leading calls per app
}

func (r *recorder) sync(_ context.Context, app string, _ []*unstructured.Unstructured) (SyncStats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[app]++
	if r.calls[app] <= r.failFirst {
		return SyncStats{}, errors.New("induced failure")
	}
	return SyncStats{}, nil
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
