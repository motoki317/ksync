package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
)

func result(group, kind, ns, name string, status common.ResultCode, hookPhase common.OperationPhase, msg string) common.ResourceSyncResult {
	return common.ResourceSyncResult{
		ResourceKey: kube.ResourceKey{Group: group, Kind: kind, Namespace: ns, Name: name},
		Status:      status,
		HookPhase:   hookPhase,
		Message:     msg,
	}
}

// stripFailedResults must drop exactly the completed-unsuccessful tasks so they
// are re-attempted, while keeping succeeded resources and completed hooks — the
// lever the convergence retry depends on. A failed hook is the subtle case: its
// Status is Synced (the Job applied) but its HookPhase is Failed (the Job ran
// and failed), so it must be dropped by HookPhase, not Status.
func TestStripFailedResults(t *testing.T) {
	okRes := result("apps", "Deployment", "team-a", "api", common.ResultCodeSynced, common.OperationSucceeded, "")
	failedApply := result("route.gateway.example.com", "Route", "team-a", "main", common.ResultCodeSyncFailed, common.OperationError, "no matches for kind")
	okHook := result("batch", "Job", "team-a", "migrate-ok", common.ResultCodeSynced, common.OperationSucceeded, "")
	failedHook := result("batch", "Job", "team-a", "migrate-bad", common.ResultCodeSynced, common.OperationFailed, "backoff limit reached")

	got := stripFailedResults([]common.ResourceSyncResult{okRes, failedApply, okHook, failedHook})

	kept := map[string]bool{}
	for _, r := range got {
		kept[r.ResourceKey.Name] = true
	}
	if !kept["api"] || !kept["migrate-ok"] {
		t.Errorf("stripFailedResults dropped a succeeded resource/hook: kept=%v", kept)
	}
	if kept["main"] {
		t.Error("stripFailedResults kept the failed apply — it will short-circuit the retry to Failed")
	}
	if kept["migrate-bad"] {
		t.Error("stripFailedResults kept the failed hook (Status Synced, HookPhase Failed) — it will not be re-run")
	}
}

// failedResult is the one "did this task fail" predicate shared by the strip,
// the retry reason, and the backoff progress set. It must catch every failure
// shape and no success: an apply failure (SyncFailed status / Error phase), a
// failed hook (Synced status but Failed phase — the case Status alone misses), a
// dry-run/permission failure (SyncFailed status, empty phase), while a succeeded
// resource, a completed-successful hook, and a still-pending task are not failed.
func TestFailedResult(t *testing.T) {
	cases := []struct {
		name string
		r    common.ResourceSyncResult
		want bool
	}{
		{"apply failure", result("route.gateway.example.com", "Route", "team-a", "main", common.ResultCodeSyncFailed, common.OperationError, "no matches for kind"), true},
		{"failed hook", result("batch", "Job", "team-a", "migrate", common.ResultCodeSynced, common.OperationFailed, "backoff limit reached"), true},
		{"dry-run failure (empty hook phase)", result("apps", "Deployment", "team-a", "api", common.ResultCodeSyncFailed, "", "admission webhook denied"), true},
		{"succeeded resource", result("apps", "Deployment", "team-a", "api", common.ResultCodeSynced, common.OperationSucceeded, ""), false},
		{"completed-successful hook", result("batch", "Job", "team-a", "migrate", common.ResultCodeSynced, common.OperationSucceeded, ""), false},
		{"pending (running, empty phase)", result("apps", "Deployment", "team-a", "api", common.ResultCodeSynced, "", ""), false},
	}
	for _, tc := range cases {
		if got := failedResult(tc.r); got != tc.want {
			t.Errorf("failedResult(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A failed PostSync hook (Status Synced, HookPhase Failed) is a supported retry
// case, so its detail must reach both the retry reason and the backoff's failing
// set — keyed off HookPhase, not Status (which stays Synced). Keyed off Status
// alone the reason went generic and the failing set was empty, resetting the
// backoff every attempt.
func TestFailureReasonAndKeys_IncludeFailedHook(t *testing.T) {
	failedHook := result("batch", "Job", "team-a", "migrate", common.ResultCodeSynced, common.OperationFailed, "backoff limit reached")

	reason := failureReason("one or more tasks failed", []common.ResourceSyncResult{failedHook})
	for _, want := range []string{"migrate", "backoff limit reached"} {
		if !strings.Contains(reason, want) {
			t.Errorf("failureReason must name the failed hook, got %q (missing %q)", reason, want)
		}
	}
	keys := failedResultKeys([]common.ResourceSyncResult{failedHook})
	if len(keys) != 1 || !strings.Contains(keys[0], "migrate") {
		t.Errorf("failedResultKeys must include the failed hook, got %v", keys)
	}
}

// missingReapply paces the re-apply of a persistently-Missing target: it fires
// on the Nth Missing poll, then backs the interval off by doubling — clamped at
// missingReapplyMax, the case that overshot to 48 before the clamp. reset returns
// it to the initial budget.
func TestMissingReapply(t *testing.T) {
	m := newMissingReapply()
	var intervals []int
	polls := 0
	for len(intervals) < 6 {
		polls++
		if m.due() {
			intervals = append(intervals, polls)
			polls = 0
		}
	}
	want := []int{3, 6, 12, 24, 30, 30} // missingReapplyPolls, doubling, clamped at missingReapplyMax
	for i := range want {
		if intervals[i] != want[i] {
			t.Errorf("re-apply interval #%d = %d polls, want %d (doubles, clamped at %d)", i, intervals[i], want[i], missingReapplyMax)
		}
	}

	m.reset()
	polls = 0
	for {
		polls++
		if m.due() {
			break
		}
	}
	if polls != missingReapplyPolls {
		t.Errorf("after reset, first re-apply at %d polls, want %d (back to the initial budget)", polls, missingReapplyPolls)
	}
}

// interruptibleWait returns nil when the delay elapses and the caller's mapped
// error when ctx ends — so a backoff never swallows a Ctrl-C or a sibling-abort.
func TestInterruptibleWait(t *testing.T) {
	if err := interruptibleWait(context.Background(), time.Millisecond, func() error { return errors.New("must not fire on elapse") }); err != nil {
		t.Errorf("elapsed wait = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mapped := errors.New("mapped")
	if err := interruptibleWait(ctx, time.Hour, func() error { return mapped }); !errors.Is(err, mapped) {
		t.Errorf("cancelled wait = %v, want the onDone-mapped error", err)
	}
}

func TestStripResultKeys(t *testing.T) {
	a := result("", "ConfigMap", "team-a", "cfg", common.ResultCodeSynced, common.OperationSucceeded, "")
	b := result("route.gateway.example.com", "Route", "team-a", "main", common.ResultCodeSynced, common.OperationSucceeded, "")

	got := stripResultKeys([]common.ResourceSyncResult{a, b}, []string{b.ResourceKey.String()})
	if len(got) != 1 || got[0].ResourceKey.Name != "cfg" {
		t.Errorf("stripResultKeys did not drop the named key: %v", got)
	}
	// An empty key set is a no-op.
	if same := stripResultKeys([]common.ResourceSyncResult{a, b}, nil); len(same) != 2 {
		t.Errorf("empty key set should not drop anything, got %d", len(same))
	}
}

func TestBackoffDelay(t *testing.T) {
	base, cap := time.Second, 30*time.Second
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		streak := i + 1
		if got := backoffDelay(base, cap, streak); got != w {
			t.Errorf("backoffDelay(streak=%d) = %s, want %s", streak, got, w)
		}
	}
}

// The backoff climbs while consecutive attempts keep failing and resets to base
// the moment an attempt makes progress (the failing set strictly shrinks), so a
// slowly-converging sync is not throttled like a wedged one. retries counts
// every failed attempt regardless, for the user-facing "(N retries)" and the
// timeout message.
func TestConvergenceBackoffAndReset(t *testing.T) {
	c := newConvergence(time.Second, 30*time.Second)
	both := []string{"Route/main", "Route/other"}

	if d := c.fail(both, "two failing"); d != time.Second {
		t.Errorf("first failure delay = %s, want 1s", d)
	}
	if d := c.fail(both, "two failing"); d != 2*time.Second {
		t.Errorf("second no-progress delay = %s, want 2s", d)
	}
	if d := c.fail(both, "two failing"); d != 4*time.Second {
		t.Errorf("third no-progress delay = %s, want 4s", d)
	}
	// One of the two resources recovered: progress → backoff resets to base.
	if d := c.fail([]string{"Route/main"}, "one failing"); d != time.Second {
		t.Errorf("delay after progress = %s, want reset to 1s", d)
	}
	if c.retries != 4 {
		t.Errorf("retries = %d, want 4 (every failed attempt counts)", c.retries)
	}
	if c.lastReason != "one failing" {
		t.Errorf("lastReason = %q, want the most recent failure", c.lastReason)
	}
}

func TestStrictSubset(t *testing.T) {
	set := func(ks ...string) map[string]struct{} { return keySet(ks) }
	cases := []struct {
		a, b map[string]struct{}
		want bool
	}{
		{set("a"), set("a", "b"), true},       // shrank
		{set("a", "b"), set("a", "b"), false}, // same — not strict
		{set("a", "c"), set("a", "b"), false}, // different member, not a subset
		{set(), set("a"), true},               // emptied — progress
		{set("a", "b"), set("a"), false},      // grew
	}
	for _, tc := range cases {
		if got := strictSubset(tc.a, tc.b); got != tc.want {
			t.Errorf("strictSubset(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestOperationFailed(t *testing.T) {
	synced := result("apps", "Deployment", "team-a", "api", common.ResultCodeSynced, common.OperationSucceeded, "")
	failed := result("route.gateway.example.com", "Route", "team-a", "main", common.ResultCodeSyncFailed, common.OperationError, "no matches for kind")

	if operationFailed(common.OperationSucceeded, []common.ResourceSyncResult{synced}) {
		t.Error("a clean Succeeded operation must not read as failed")
	}
	if !operationFailed(common.OperationFailed, nil) {
		t.Error("a Failed phase must read as failed")
	}
	if !operationFailed(common.OperationSucceeded, []common.ResourceSyncResult{synced, failed}) {
		t.Error("a SyncFailed result must read as failed even when the phase is Succeeded")
	}
	failedHook := result("batch", "Job", "team-a", "migrate", common.ResultCodeSynced, common.OperationFailed, "backoff limit reached")
	if !operationFailed(common.OperationSucceeded, []common.ResourceSyncResult{failedHook}) {
		t.Error("a failed hook (Status Synced, HookPhase Failed) must read as failed")
	}
}

// notConverged distinguishes the app's own deadline (a real timeout → the typed
// error the command layer turns into a diagnostic dump) from any other
// cancellation (Ctrl-C or a sibling app's failure aborting the run → a clean
// interrupt). Mis-mapping either way is a misleading exit.
func TestNotConverged(t *testing.T) {
	timeoutErr := &TimeoutError{App: "shop"}

	deadline, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	<-deadline.Done()
	if got := notConverged(deadline, timeoutErr); got != error(timeoutErr) {
		t.Errorf("on DeadlineExceeded, notConverged = %v, want the timeout error", got)
	}

	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if got := notConverged(cancelled, timeoutErr); !errors.Is(got, context.Canceled) {
		t.Errorf("on Canceled, notConverged = %v, want the cancellation (not a timeout)", got)
	}
}

// FailFast (destroy) surfaces the failure reason verbatim, and never an empty
// message even when the engine supplied none.
func TestFailFastError(t *testing.T) {
	if got := failFastError("webhook denied the delete").Error(); got != "webhook denied the delete" {
		t.Errorf("failFastError = %q, want the reason verbatim", got)
	}
	if got := failFastError("").Error(); got == "" {
		t.Error("failFastError must never produce an empty message")
	}
}

func TestFailureReason(t *testing.T) {
	failed := result("route.gateway.example.com", "Route", "team-a", "main", common.ResultCodeSyncFailed, common.OperationError, `no matches for kind "Route"`)
	other := result("apps", "Deployment", "team-a", "api", common.ResultCodeSyncFailed, common.OperationError, "admission webhook denied")

	// Per-resource failures win over the phase message and stay on one line.
	got := failureReason("one or more tasks failed", []common.ResourceSyncResult{failed, other})
	for _, want := range []string{"Route", "main", "no matches for kind", "Deployment", "admission webhook denied"} {
		if !strings.Contains(got, want) {
			t.Errorf("failureReason %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("failureReason must stay one logical line, got %q", got)
	}
	// With no failed result, fall back to the phase message.
	if got := failureReason("namespace not found", nil); got != "namespace not found" {
		t.Errorf("failureReason fallback = %q, want the phase message", got)
	}
}

func TestHasMissingAndKeys(t *testing.T) {
	pending := []ResourceStatus{
		{Group: "apps", Kind: "Deployment", Namespace: "team-a", Name: "api", Status: "Progressing"},
		{Group: "route.gateway.example.com", Kind: "Route", Namespace: "team-a", Name: "main", Status: "Missing", Message: "not yet created"},
	}
	if !hasMissing(pending) {
		t.Error("hasMissing should see the Missing Route")
	}
	keys := missingKeys(pending)
	if len(keys) != 1 || !strings.Contains(keys[0], "Route") || !strings.Contains(keys[0], "main") {
		t.Errorf("missingKeys = %v, want just the Route", keys)
	}
	if hasMissing([]ResourceStatus{{Kind: "Deployment", Status: "Progressing"}}) {
		t.Error("hasMissing must be false when nothing is Missing")
	}
}

// The timeout message must name the retry count and the last real failure, so a
// sync that timed out after repeated apply failures reports the cause, not just
// the symptom — while the no-retry cases keep their original wording (existing
// callers and tests depend on it).
func TestTimeoutError_ReportsRetriesAndLastFailure(t *testing.T) {
	withRetries := &TimeoutError{
		App:         "shop",
		Retries:     4,
		LastFailure: `route.gateway.example.com/Route/team-a/main: no matches for kind "Route"`,
		Pending:     []ResourceStatus{{Kind: "Route", Namespace: "team-a", Name: "main", Status: "Missing", Message: "not yet created"}},
	}
	msg := withRetries.Error()
	for _, want := range []string{"shop", "timed out", "after 4 retries", "last failure", "no matches for kind", "still not healthy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("timeout message %q missing %q", msg, want)
		}
	}

	// One retry reads "1 retry" (singular).
	if one := (&TimeoutError{App: "shop", Retries: 1, LastFailure: "x"}).Error(); !strings.Contains(one, "after 1 retry") || strings.Contains(one, "retries") {
		t.Errorf("single-retry message %q should say '1 retry'", one)
	}

	// No retries, no pending: the original wording is preserved.
	if none := (&TimeoutError{App: "shop"}).Error(); none != `sync of "shop" timed out before converging` {
		t.Errorf("no-retry message = %q, want the original", none)
	}
}
