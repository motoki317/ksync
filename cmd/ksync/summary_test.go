package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/loop"
	"github.com/motoki317/ksync/internal/ui"
)

func synced(kind, name string, code common.ResultCode) common.ResourceSyncResult {
	return common.ResourceSyncResult{
		ResourceKey: kube.ResourceKey{Group: "apps", Kind: kind, Namespace: "api-b", Name: name},
		Status:      code,
	}
}

// A clean app prints ✓ and just the applied count.
func TestPrintSummary_CleanIsCheckmark(t *testing.T) {
	var b bytes.Buffer
	results := []common.ResourceSyncResult{
		synced("Deployment", "api", common.ResultCodeSynced),
		synced("Service", "api", common.ResultCodeSynced),
	}
	printSummary(&b, ui.NewColors(&b), "shop", results, nil, 1500*time.Millisecond)
	out := b.String()
	if !strings.Contains(out, "✓") || !strings.Contains(out, "shop") || !strings.Contains(out, "2 applied") {
		t.Errorf("clean summary = %q, want ✓ shop 2 applied", out)
	}
	// The per-app apply duration is appended (the watch loop and `ksync sync` both
	// report it on the 🚢 line).
	if !strings.Contains(out, "1.5s") {
		t.Errorf("clean summary = %q, want the apply duration 1.5s", out)
	}
	if strings.Contains(out, "degraded") || strings.Contains(out, "⚠") {
		t.Errorf("clean summary must not mention degraded: %q", out)
	}
}

// A degraded resource downgrades the symbol to ⚠, adds the count, and lists the
// offending resource under the app line — without it counting as a sync failure.
func TestPrintSummary_DegradedIsWarning(t *testing.T) {
	var b bytes.Buffer
	results := []common.ResourceSyncResult{synced("Deployment", "api", common.ResultCodeSynced)}
	degraded := []string{"apps/Deployment/api-b/api: Degraded — progress deadline exceeded"}
	printSummary(&b, ui.NewColors(&b), "shop", results, degraded, 0)
	out := b.String()
	if !strings.Contains(out, "⚠") {
		t.Errorf("degraded summary = %q, want ⚠", out)
	}
	if !strings.Contains(out, "1 applied") || !strings.Contains(out, "1 degraded") {
		t.Errorf("degraded summary = %q, want 1 applied, 1 degraded", out)
	}
	if !strings.Contains(out, "progress deadline exceeded") {
		t.Errorf("degraded summary = %q, want the degraded resource detail line", out)
	}
}

// A sync failure outranks degraded: the line is ✗.
func TestPrintSummary_FailedIsCross(t *testing.T) {
	var b bytes.Buffer
	results := []common.ResourceSyncResult{
		synced("Deployment", "api", common.ResultCodeSyncFailed),
	}
	printSummary(&b, ui.NewColors(&b), "shop", results, nil, 0)
	if out := b.String(); !strings.Contains(out, "✗") || !strings.Contains(out, "1 failed") {
		t.Errorf("failed summary = %q, want ✗ ... 1 failed", out)
	}
}

// The committed (final) summary block is titled, names the degraded apps, shows
// the absolute synced count and duration — and drops Context / Start at.
func TestSummaryLines_FinalNamesDegraded(t *testing.T) {
	c := ui.NewColors(&bytes.Buffer{}) // not a TTY → plain, assertable text
	got := strings.Join(summaryLines(c, 18, 16, loop.SyncStats{Degraded: 2}, []string{"api-b", "shop"}, 1600*time.Millisecond, true), "\n")
	for _, want := range []string{"Summary", "16 synced", "2 degraded", "api-b, shop", "Duration", "1.6s"} {
		if !strings.Contains(got, want) {
			t.Errorf("final summary = %q, want it to contain %q", got, want)
		}
	}
	for _, gone := range []string{"Context", "Start at"} {
		if strings.Contains(got, gone) {
			t.Errorf("final summary = %q, should no longer contain %q", got, gone)
		}
	}
}

// The live footer shows running progress ("k/N synced") and stays short — it
// never names the degraded apps, so a long list cannot wrap the pinned block.
func TestSummaryLines_LiveIsProgressAndShort(t *testing.T) {
	c := ui.NewColors(&bytes.Buffer{})
	got := strings.Join(summaryLines(c, 18, 12, loop.SyncStats{Degraded: 1}, []string{"shop"}, 800*time.Millisecond, false), "\n")
	if !strings.Contains(got, "12/18 synced") || !strings.Contains(got, "1 degraded") {
		t.Errorf("live summary = %q, want 12/18 synced and 1 degraded", got)
	}
	if strings.Contains(got, "shop") {
		t.Errorf("live summary must not name degraded apps (wrap risk): %q", got)
	}
}

func TestSummaryLines_OmitsDegradedWhenZero(t *testing.T) {
	c := ui.NewColors(&bytes.Buffer{})
	got := strings.Join(summaryLines(c, 16, 16, loop.SyncStats{}, nil, 1600*time.Millisecond, true), "\n")
	if strings.Contains(got, "degraded") {
		t.Errorf("clean summary must omit degraded: %q", got)
	}
}

// The plan is titled and opens a whole-stack sync with the count, the context,
// and the names.
func TestPrintPlan_ListsScope(t *testing.T) {
	var b bytes.Buffer
	apps := []config.App{{Name: "api-b"}, {Name: "shop"}, {Name: "team-a"}}
	printPlan(&b, ui.NewColors(&b), apps, "prod-cluster")
	out := b.String()
	for _, want := range []string{"Plan", "3 apps", "prod-cluster", "api-b", "shop", "team-a"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan = %q, want it to contain %q", out, want)
		}
	}
}
