package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"

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

// The run summary closes a multi-app sync with app counts and wall time.
func TestPrintRunSummary_CountsAppsAndDegraded(t *testing.T) {
	var b bytes.Buffer
	printRunSummary(&b, ui.NewColors(&b), 16, loop.SyncStats{Degraded: 2}, 1600*time.Millisecond)
	out := b.String()
	if !strings.Contains(out, "16 synced") || !strings.Contains(out, "2 degraded") || !strings.Contains(out, "1.6s") {
		t.Errorf("run summary = %q, want 16 synced · 2 degraded · 1.6s", out)
	}
}

func TestPrintRunSummary_OmitsDegradedWhenZero(t *testing.T) {
	var b bytes.Buffer
	printRunSummary(&b, ui.NewColors(&b), 16, loop.SyncStats{}, 1600*time.Millisecond)
	if out := b.String(); strings.Contains(out, "degraded") {
		t.Errorf("clean run summary must omit degraded: %q", out)
	}
}
