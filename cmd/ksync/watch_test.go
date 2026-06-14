package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/loop"
	"github.com/motoki317/ksync/internal/ui"
)

// A whole-stack watch frames its startup like a sync run: a Plan up front, then
// the committed Summary once every app has synced once — and not before.
func TestWatchReporter_CommitsSummaryAtInitialConvergence(t *testing.T) {
	var buf bytes.Buffer
	apps := []config.App{{Name: "api-b"}, {Name: "shop"}}
	r := startWatchReporter(&buf, ui.NewColors(&buf), apps, "prod-cluster")

	if out := buf.String(); !strings.Contains(out, "Plan") || !strings.Contains(out, "prod-cluster") {
		t.Fatalf("watch start = %q, want a Plan naming the context", out)
	}

	r.report("api-b", loop.SyncStats{Applied: 2}, 0)
	if strings.Contains(buf.String(), "Summary") {
		t.Errorf("Summary committed before every app synced once:\n%s", buf.String())
	}

	r.report("shop", loop.SyncStats{Applied: 1}, 0)
	if out := buf.String(); !strings.Contains(out, "Summary") || !strings.Contains(out, "2 synced") {
		t.Errorf("at convergence, want a committed Summary with 2 synced, got:\n%s", out)
	}

	// Incremental syncs after convergence stream their ship line but never
	// re-commit a Summary block — that would be per-keystroke noise.
	commits := strings.Count(buf.String(), "Summary")
	r.report("api-b", loop.SyncStats{Applied: 1}, 40*time.Millisecond)
	if got := strings.Count(buf.String(), "Summary"); got != commits {
		t.Errorf("incremental sync re-committed Summary: %d -> %d", commits, got)
	}
}

// A single-app watch skips the Plan/Summary framing — like a one-app sync, the
// one ship line says it all.
func TestWatchReporter_SingleAppHasNoPlanOrSummary(t *testing.T) {
	var buf bytes.Buffer
	apps := []config.App{{Name: "duo"}}
	r := startWatchReporter(&buf, ui.NewColors(&buf), apps, "prod-cluster")
	r.report("duo", loop.SyncStats{Applied: 3}, 0)
	out := buf.String()
	if strings.Contains(out, "Plan") || strings.Contains(out, "Summary") {
		t.Errorf("single-app watch must not frame Plan/Summary: %q", out)
	}
	if !strings.Contains(out, "duo") || !strings.Contains(out, "3 applied") {
		t.Errorf("single-app watch = %q, want the ship line", out)
	}
}
