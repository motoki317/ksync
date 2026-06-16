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
// a committed Summary once the batch settles (onIdle) — not on each app's report
// — followed by a watching-for-changes line. A later rebuild commits its own
// Summary, so the developer sees the result of every batch, not just the first.
func TestWatchReporter_CommitsSummaryPerBatch(t *testing.T) {
	var buf bytes.Buffer
	apps := []config.App{{Name: "api-b"}, {Name: "shop"}}
	out := ui.NewColors(&buf)
	log := ui.New(ui.Options{Writer: &buf})
	r := startWatchReporter(&buf, out, log, newProgress(&buf, out, apps), apps, "prod-cluster")

	if out := buf.String(); !strings.Contains(out, "Plan") || !strings.Contains(out, "prod-cluster") {
		t.Fatalf("watch start = %q, want a Plan naming the context", out)
	}

	// Reports tally the batch but do not commit the Summary on their own.
	r.report("api-b", loop.SyncStats{Applied: 2}, 0)
	r.report("shop", loop.SyncStats{Applied: 1}, 0)
	if strings.Contains(buf.String(), "Summary") {
		t.Errorf("Summary committed before the batch settled:\n%s", buf.String())
	}

	// Settling to idle commits the convergence Summary and logs the watching line.
	r.onIdle(24 * time.Second)
	if out := buf.String(); !strings.Contains(out, "Summary") || !strings.Contains(out, "2 synced") {
		t.Errorf("at convergence, want a committed Summary with 2 synced, got:\n%s", out)
	}
	if !strings.Contains(buf.String(), "watching for changes") {
		t.Errorf("convergence should log a watching-for-changes line, got:\n%s", buf.String())
	}

	// A later rebuild batch commits its own Summary, counting only that batch.
	commits := strings.Count(buf.String(), "Summary")
	r.report("api-b", loop.SyncStats{Applied: 1}, 40*time.Millisecond)
	r.onIdle(1500 * time.Millisecond)
	out2 := buf.String()
	if got := strings.Count(out2, "Summary"); got != commits+1 {
		t.Errorf("a rebuild batch should commit its own Summary: %d -> %d", commits, got)
	}
	if !strings.Contains(out2, "1 synced") {
		t.Errorf("the rebuild Summary should count only that batch (1 synced), got:\n%s", out2)
	}
}

// A single-app watch skips the Plan/Summary framing — like a one-app sync, the
// one ship line says it all — but still logs a watching-for-changes line so the
// developer knows the loop is idle and ready.
func TestWatchReporter_SingleAppHasNoPlanOrSummary(t *testing.T) {
	var buf bytes.Buffer
	apps := []config.App{{Name: "duo"}}
	colors := ui.NewColors(&buf)
	log := ui.New(ui.Options{Writer: &buf})
	r := startWatchReporter(&buf, colors, log, newProgress(&buf, colors, apps), apps, "prod-cluster")
	r.report("duo", loop.SyncStats{Applied: 3}, 0)
	r.onIdle(time.Second)
	out := buf.String()
	if strings.Contains(out, "Plan") || strings.Contains(out, "Summary") {
		t.Errorf("single-app watch must not frame Plan/Summary: %q", out)
	}
	if !strings.Contains(out, "duo") || !strings.Contains(out, "3 applied") {
		t.Errorf("single-app watch = %q, want the ship line", out)
	}
	if !strings.Contains(out, "watching for changes") {
		t.Errorf("single-app watch should still log a watching-for-changes line: %q", out)
	}
}
