package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
)

// TestStartEagerBuilds_BuildsConcurrentlyIgnoringNeeds proves the one-shot sync
// builds ahead of the needs DAG: every build-app's image starts building at once
// — even a dependent's, which does not wait for the app it needs — and await
// returns each build's tags. A build-less app returns a zero outcome at once.
func TestStartEagerBuilds_BuildsConcurrentlyIgnoringNeeds(t *testing.T) {
	apps := []config.App{
		{Name: "base", Build: []config.Build{{Image: "img-base"}}},
		{Name: "app", Needs: []string{"base"}, Build: []config.Build{{Image: "img-app"}}},
		{Name: "plain"}, // build-less
	}
	var peak int32
	release := make(chan struct{})
	buildFn := func(_ context.Context, _ string, builds []config.Build) ([]string, error) {
		atomic.AddInt32(&peak, 1)
		<-release // hold so both builds are seen in flight together
		refs := make([]string, len(builds))
		for i, b := range builds {
			refs[i] = b.Image + ":ksync-000000000001"
		}
		return refs, nil
	}

	await, wait := startEagerBuilds(context.Background(), apps, buildFn, 4, nil)
	// Both build-apps must reach buildFn without anyone releasing them — builds
	// are needs-free, so `app` does not wait for `base`. Poll, then release.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&peak) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if got := atomic.LoadInt32(&peak); got < 2 {
		t.Errorf("concurrent builds = %d, want 2 (builds must not be serialized behind the needs DAG)", got)
	}

	// BuildAll stores the tag portion of each ref (build.Tag strips the name).
	if got := await("base"); got.err != nil || got.tags[0] != "ksync-000000000001" {
		t.Errorf("base outcome = %+v, want the built tag", got)
	}
	if got := await("app"); got.err != nil || got.tags[0] != "ksync-000000000001" {
		t.Errorf("app outcome = %+v, want the built tag", got)
	}
	if got := await("plain"); got.tags != nil || got.err != nil {
		t.Errorf("build-less app outcome = %+v, want zero", got)
	}
	wait()
}

// TestStartEagerBuilds_SkipsOverriddenImages proves the one-shot sync builds
// only non-overridden entries: a mixed app builds its un-supplied image and
// leaves the overridden one to deploy-time injection, and an all-overridden app
// has nothing to build (a zero outcome at once).
func TestStartEagerBuilds_SkipsOverriddenImages(t *testing.T) {
	apps := []config.App{
		{Name: "duo", Build: []config.Build{{Image: "img-api"}, {Image: "img-ui"}}},
		{Name: "all-ovr", Build: []config.Build{{Image: "img-x"}}},
	}
	var mu sync.Mutex
	var built []string
	buildFn := func(_ context.Context, _ string, builds []config.Build) ([]string, error) {
		mu.Lock()
		refs := make([]string, len(builds))
		for i, b := range builds {
			built = append(built, b.Image)
			refs[i] = b.Image + ":ksync-000000000001"
		}
		mu.Unlock()
		return refs, nil
	}
	overrides := map[string]render.Image{
		"img-ui": {Name: "img-ui", NewTag: "supplied"},
		"img-x":  {Name: "img-x", NewTag: "supplied"},
	}
	await, wait := startEagerBuilds(context.Background(), apps, buildFn, 4, overrides)

	bo := await("duo")
	if bo.err != nil {
		t.Fatalf("duo outcome err: %v", bo.err)
	}
	if _, ok := bo.tags[0]; !ok {
		t.Errorf("duo entry 0 (img-api) should be built: %+v", bo.tags)
	}
	if _, ok := bo.tags[1]; ok {
		t.Errorf("duo entry 1 (img-ui) is overridden and must not be built: %+v", bo.tags)
	}
	if bo := await("all-ovr"); bo.tags != nil || bo.err != nil {
		t.Errorf("all-overridden app outcome = %+v, want zero", bo)
	}
	wait()

	mu.Lock()
	defer mu.Unlock()
	if len(built) != 1 || built[0] != "img-api" {
		t.Errorf("built = %v, want only [img-api]", built)
	}
}

// TestStartEagerBuilds_PropagatesBuildError asserts a build failure surfaces
// through await, so the deploy phase can fail the app and cancel its dependents.
func TestStartEagerBuilds_PropagatesBuildError(t *testing.T) {
	apps := []config.App{{Name: "a", Build: []config.Build{{Image: "img-a"}}}}
	buildFn := func(context.Context, string, []config.Build) ([]string, error) {
		return nil, errors.New("induced build failure")
	}
	await, wait := startEagerBuilds(context.Background(), apps, buildFn, 1, nil)
	if got := await("a"); got.err == nil {
		t.Error("await returned no error for a failed build; the deploy would apply an unbuilt image")
	}
	wait()
}
