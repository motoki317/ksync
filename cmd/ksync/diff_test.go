package main

import (
	"io"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/render"
	"github.com/motoki317/ksync/internal/ui"
)

func TestDiffImages(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{
		{Name: "team-a", Build: []config.Build{{Image: "ghcr.io/app/seed"}, {Image: "ghcr.io/app/worker"}, {Image: "ghcr.io/app/unused"}}},
		{Name: "api-b", Needs: []string{"team-a"}, Build: []config.Build{{Image: "ghcr.io/app/api-b"}}},
		{Name: "shop", Needs: []string{"api-b"}},
		{Name: "plain"},
	}}
	for _, tt := range []struct {
		name         string
		app          string
		alsoSelect   []string
		deployed     map[string]bool
		overrides    map[string]render.Image
		wantSupplied []render.Image
		wantRepos    []string
	}{
		{
			name: "unselected owner leaves needed image at manifest ref", app: "api-b",
			deployed:  map[string]bool{"ghcr.io/app/api-b": true, "ghcr.io/app/seed": true},
			wantRepos: []string{"ghcr.io/app/api-b"},
		},
		{
			name: "unselected owner supplies only overridden images", app: "api-b",
			deployed: map[string]bool{"ghcr.io/app/api-b": true, "ghcr.io/app/seed": true, "ghcr.io/app/worker": true},
			overrides: map[string]render.Image{
				"ghcr.io/app/seed": {Name: "ghcr.io/app/seed", NewName: "ghcr.io/app/seed-copy", NewTag: "supplied"},
			},
			wantSupplied: []render.Image{{Name: "ghcr.io/app/seed", NewName: "ghcr.io/app/seed-copy", NewTag: "supplied"}},
			wantRepos:    []string{"ghcr.io/app/api-b"},
		},
		{
			name: "own and needed referenced images", app: "api-b",
			alsoSelect: []string{"team-a"},
			deployed:   map[string]bool{"ghcr.io/app/api-b": true, "ghcr.io/app/seed": true, "ghcr.io/app/worker": true},
			wantRepos:  []string{"ghcr.io/app/api-b", "ghcr.io/app/seed", "ghcr.io/app/worker"},
		},
		{
			name: "own and needed overrides take precedence", app: "api-b",
			alsoSelect: []string{"team-a"},
			deployed:   map[string]bool{"ghcr.io/app/api-b": true, "ghcr.io/app/seed": true, "ghcr.io/app/worker": true},
			overrides: map[string]render.Image{
				"ghcr.io/app/api-b": {Name: "ghcr.io/app/api-b", NewTag: "supplied"},
				"ghcr.io/app/seed":  {Name: "ghcr.io/app/seed", NewName: "ghcr.io/app/seed-copy", Digest: "sha256:0123456789012345678901234567890123456789012345678901234567890123"},
			},
			wantSupplied: []render.Image{
				{Name: "ghcr.io/app/api-b", NewTag: "supplied"},
				{Name: "ghcr.io/app/seed", NewName: "ghcr.io/app/seed-copy", Digest: "sha256:0123456789012345678901234567890123456789012345678901234567890123"},
			},
			wantRepos: []string{"ghcr.io/app/worker"},
		},
		{
			name: "unreferenced needed override still supplies its ref", app: "api-b",
			overrides:    map[string]render.Image{"ghcr.io/app/unused": {Name: "ghcr.io/app/unused", NewTag: "supplied"}},
			wantSupplied: []render.Image{{Name: "ghcr.io/app/unused", NewTag: "supplied"}},
		},
		{
			name: "own builds retain their behavior", app: "team-a",
			deployed:     map[string]bool{"ghcr.io/app/seed": true, "ghcr.io/app/worker": true},
			overrides:    map[string]render.Image{"ghcr.io/app/worker": {Name: "ghcr.io/app/worker", NewTag: "supplied"}},
			wantSupplied: []render.Image{{Name: "ghcr.io/app/worker", NewTag: "supplied"}},
			wantRepos:    []string{"ghcr.io/app/seed"},
		},
		{
			name: "transitive owner supplies neither overrides nor carry-forward", app: "shop",
			alsoSelect: []string{"api-b", "team-a"},
			deployed:   map[string]bool{"ghcr.io/app/api-b": true, "ghcr.io/app/seed": true},
			overrides:  map[string]render.Image{"ghcr.io/app/seed": {Name: "ghcr.io/app/seed", NewTag: "supplied"}},
			wantRepos:  []string{"ghcr.io/app/api-b"},
		},
		{
			name: "unrelated build is ignored", app: "plain",
			deployed:  map[string]bool{"ghcr.io/app/seed": true},
			overrides: map[string]render.Image{"ghcr.io/app/seed": {Name: "ghcr.io/app/seed", NewTag: "supplied"}},
		},
		{name: "no referenced images", app: "api-b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			apps, err := cfg.Select(append([]string{tt.app}, tt.alsoSelect...), nil)
			if err != nil {
				t.Fatal(err)
			}
			selected := make(map[string]bool, len(apps))
			for _, app := range apps {
				selected[app.Name] = true
			}
			app := apps[0]
			supplied, repos, carried := diffImages(app, cfg.NeededBuilds(app), selected, tt.deployed, tt.overrides, nil)
			if !slices.Equal(supplied, tt.wantSupplied) {
				t.Errorf("supplied = %+v, want %+v", supplied, tt.wantSupplied)
			}
			if !slices.Equal(repos, tt.wantRepos) {
				t.Errorf("build repos = %v, want %v", repos, tt.wantRepos)
			}
			if len(carried) != 0 {
				t.Errorf("carried repos without owner tags = %v, want none", carried)
			}
		})
	}
}

func TestDiffImages_SelectedOwnerDrift(t *testing.T) {
	const image = "ghcr.io/app/seed"
	const oldTag = "ksync-000000000001"
	const newTag = "ksync-000000000002"
	app := config.App{Name: "api-b"}
	needed := []config.BuildRef{{App: "team-a", Image: image}}
	for _, tt := range []struct {
		name     string
		live     string
		override string
		want     string
	}{
		{name: "owner live tag wins over consumer", live: newTag, want: newTag},
		{name: "missing owner tag falls back to consumer", want: oldTag},
		{name: "non-dev owner tag falls back to consumer", live: "stable", want: oldTag},
		{name: "explicit override wins over owner", live: newTag, override: "supplied", want: "supplied"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			live := func(tag string) []*unstructured.Unstructured {
				return []*unstructured.Unstructured{{Object: map[string]any{
					"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "api-b"},
					"spec": map[string]any{"containers": []any{map[string]any{"name": "seed", "image": image + ":" + tag}}},
				}}}
			}
			res := renderNeededImage(t, image+":manifest")
			var ownerLive []*unstructured.Unstructured
			if tt.live != "" {
				ownerLive = live(tt.live)
			}
			ownerTags := map[string]map[string]string{"team-a": render.LiveDevTags(ownerLive, []string{image})}
			var overrides map[string]render.Image
			if tt.override != "" {
				overrides = map[string]render.Image{image: {Name: image, NewTag: tt.override}}
			}
			supplied, repos, carried := diffImages(app, needed, map[string]bool{"team-a": true}, res.ReferencedRepos(), overrides, ownerTags)
			var wantCarried []string
			if tt.live == newTag && tt.override == "" {
				wantCarried = []string{image}
			}
			if !slices.Equal(carried, wantCarried) {
				t.Errorf("owner-carried repos = %v, want %v", carried, wantCarried)
			}
			if err := res.SetImages(supplied); err != nil {
				t.Fatal(err)
			}
			if _, err := res.CarryForwardBuildTags(live(oldTag), repos); err != nil {
				t.Fatal(err)
			}
			if got := res.Images(); !slices.Equal(got, []string{image + ":" + tt.want}) {
				t.Errorf("predicted image = %v, want %s:%s", got, image, tt.want)
			}
		})
	}
}

func plainColors() ui.Colors { return ui.NewColors(io.Discard) } // io.Discard is not a TTY → no escapes

// formatDiff lays out a per-app section for each app with changes (resource
// headers plus colorized hunks and a carried-forward note), names the in-sync
// apps once, and closes with a count.
func TestFormatDiff_SectionsAndSummary(t *testing.T) {
	results := []appDiff{
		{
			app: config.App{Name: "api-b"},
			diffs: []engine.ResourceDiff{
				{Kind: "Deployment", Namespace: "team-a", Name: "api-b", Type: engine.DiffUpdate,
					Before: "spec:\n  replicas: 1\n", After: "spec:\n  replicas: 2\n"},
				{Kind: "ConfigMap", Namespace: "team-a", Name: "new", Type: engine.DiffCreate,
					After: "data:\n  k: v\n"},
			},
			carried: []string{"example.com/team-a/api-b"},
		},
		{app: config.App{Name: "db"}}, // in sync
	}
	got := formatDiff(plainColors(), results)

	for _, want := range []string{
		"api-b",
		"~ Deployment/api-b  team-a  update",
		"+ ConfigMap/new  team-a  create",
		"-  replicas: 1",
		"+  replicas: 2",
		"note: carried forward live dev tag for example.com/team-a/api-b (not rebuilt)",
		"In sync: db",
		"Summary  1 to create, 1 to update, 0 to prune",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatDiff output missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// With no changes anywhere, the output is a single reassuring line — not an empty
// string or a zero summary.
func TestFormatDiff_NoChanges(t *testing.T) {
	got := formatDiff(plainColors(), []appDiff{{app: config.App{Name: "api-b"}}, {app: config.App{Name: "db"}}})
	if !strings.Contains(got, "No changes") {
		t.Errorf("want a no-changes line, got %q", got)
	}
	if strings.Contains(got, "Summary") {
		t.Errorf("no-changes output must not print a summary count: %q", got)
	}
}

// A build image with no live dev tag (carry-forward fell open) shows at its
// source ref, so the section warns the displayed tag is not what a sync deploys.
func TestFormatDiff_NotesBuildImageShownAtSourceRef(t *testing.T) {
	got := formatDiff(plainColors(), []appDiff{{
		app: config.App{Name: "api-b"},
		diffs: []engine.ResourceDiff{
			{Kind: "Deployment", Namespace: "team-a", Name: "api-b", Type: engine.DiffCreate, After: "spec: {}\n"},
		},
		atSrcRef: []string{"example.com/team-a/api-b"},
	}})
	want := "note: example.com/team-a/api-b shown at source ref; sync rebuilds to a fresh dev tag"
	if !strings.Contains(got, want) {
		t.Errorf("missing source-ref note %q\n--- got ---\n%s", want, got)
	}
}

// A fail-open build app with no resource hunks (live already at the source ref)
// is still not "in sync": a sync rebuilds it to a fresh ksync tag the diff cannot
// predict. Its section and note must print, and it must not be named in sync nor
// trigger the global "No changes" line.
func TestFormatDiff_FailOpenBuildAppIsNotInSync(t *testing.T) {
	got := formatDiff(plainColors(), []appDiff{
		{app: config.App{Name: "api-b"}, atSrcRef: []string{"example.com/team-a/api-b"}}, // no diffs
		{app: config.App{Name: "db"}}, // genuinely in sync
	})
	if strings.Contains(got, "No changes") {
		t.Errorf("a fail-open build app must suppress the global no-changes line: %q", got)
	}
	if !strings.Contains(got, "api-b") || !strings.Contains(got, "shown at source ref") {
		t.Errorf("fail-open app section/note missing: %q", got)
	}
	if strings.Contains(got, "In sync: api-b") {
		t.Errorf("fail-open app must not be listed in sync: %q", got)
	}
	if !strings.Contains(got, "In sync: db") {
		t.Errorf("genuinely in-sync app should still be named: %q", got)
	}
}

// A prune candidate renders as an all-removed block under a delete header.
func TestResourceDiffBlock_Prune(t *testing.T) {
	got := resourceDiffBlock(plainColors(), engine.ResourceDiff{
		Kind: "Service", Namespace: "team-a", Name: "old", Type: engine.DiffPrune,
		Before: "spec:\n  type: ClusterIP\n",
	})
	if !strings.Contains(got, "- Service/old  team-a  prune") {
		t.Errorf("missing prune header: %s", got)
	}
	if !strings.Contains(got, "-spec:") || !strings.Contains(got, "-  type: ClusterIP") {
		t.Errorf("prune body should be all-removed lines: %s", got)
	}
}
