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
	printSummary(&b, ui.NewColors(&b), "shop", results, nil)
	out := b.String()
	if !strings.Contains(out, "✓") || !strings.Contains(out, "shop") || !strings.Contains(out, "2 applied") {
		t.Errorf("clean summary = %q, want ✓ shop 2 applied", out)
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
	printSummary(&b, ui.NewColors(&b), "shop", results, degraded)
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
	printSummary(&b, ui.NewColors(&b), "shop", results, nil)
	if out := b.String(); !strings.Contains(out, "✗") || !strings.Contains(out, "1 failed") {
		t.Errorf("failed summary = %q, want ✗ ... 1 failed", out)
	}
}

// The run summary closes a multi-app sync with a standout block: apps synced,
// degraded apps (named), the context, and the duration.
func TestPrintRunSummary_CountsAppsAndDegraded(t *testing.T) {
	var b bytes.Buffer
	printRunSummary(&b, ui.NewColors(&b), runResult{
		synced: 16, agg: loop.SyncStats{Degraded: 2}, degradedApps: []string{"api-b", "shop"},
		context: "prod-cluster", start: time.Unix(0, 0).UTC(), took: 1600 * time.Millisecond,
	})
	out := b.String()
	for _, want := range []string{"16 synced", "2 degraded", "api-b, shop", "prod-cluster", "1.6s", "Duration"} {
		if !strings.Contains(out, want) {
			t.Errorf("run summary = %q, want it to contain %q", out, want)
		}
	}
}

func TestPrintRunSummary_OmitsDegradedWhenZero(t *testing.T) {
	var b bytes.Buffer
	printRunSummary(&b, ui.NewColors(&b), runResult{
		synced: 16, context: "prod-cluster", start: time.Unix(0, 0).UTC(), took: 1600 * time.Millisecond,
	})
	if out := b.String(); strings.Contains(out, "degraded") {
		t.Errorf("clean run summary must omit degraded: %q", out)
	}
}

// The plan opens a whole-stack sync with the count, the context, and the names.
func TestPrintPlan_ListsScope(t *testing.T) {
	var b bytes.Buffer
	apps := []config.App{{Name: "api-b"}, {Name: "shop"}, {Name: "team-a"}}
	printPlan(&b, ui.NewColors(&b), apps, "prod-cluster")
	out := b.String()
	for _, want := range []string{"sync 3 apps", "prod-cluster", "api-b", "shop", "team-a"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan = %q, want it to contain %q", out, want)
		}
	}
}
