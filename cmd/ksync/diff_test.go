package main

import (
	"io"
	"strings"
	"testing"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/ui"
)

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
