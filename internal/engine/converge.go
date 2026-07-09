package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
)

// Retry/backoff bounds for the convergence loop. A failing sync is retried
// until it applies-and-heals or --timeout expires — there is no attempt-count
// bound — backing off between attempts so it never hot-loops against a slow
// admission webhook or a CRD still registering.
const (
	retryBase = time.Second
	retryCap  = 30 * time.Second
)

// Missing-target re-apply pacing (see the health gate in Sync). A target still
// Missing after a successful apply is re-applied, but only after it has stayed
// Missing across a few health polls — so a resource merely not yet observed by
// the cache on the clean path is never re-applied — and the poll budget backs
// off so a wedged resource does not re-apply on a hot loop.
const (
	missingReapplyPolls = 3  // consecutive Missing polls (~3s) before the first re-apply
	missingReapplyMax   = 30 // cap on the poll budget between re-applies (~30s)
)

// RetryEvent describes one convergence retry so the command layer can render a
// failure or recovery line. UI-neutral (no gitops-engine types), like
// ResourceStatus, so the command layer needs no engine-internal imports.
type RetryEvent struct {
	// Attempt is the 1-based number of the attempt that just failed. On a
	// Recovered event it is the number of retries the recovery took.
	Attempt int
	// NextDelay is the backoff ksync waits before the next attempt. Zero on a
	// Recovered event.
	NextDelay time.Duration
	// Reason is the representative failure, "Kind/name: message", passed through
	// verbatim (no truncation). Empty on a Recovered event. It is also the
	// command layer's dedupe key.
	Reason string
	// Recovered marks the single event emitted when a sync first applies cleanly
	// after >=1 failed attempt.
	Recovered bool
	// Retries is the running total of retries for this sync so far, for the
	// Timings "(N retries)" suffix.
	Retries int
}

// convergence tracks the retry/backoff state shared across a Sync's apply
// attempts. The backoff grows while consecutive attempts keep failing and
// resets when an attempt makes progress (the failing-resource set strictly
// shrinks), so a sync that is slowly converging is not penalized like one that
// is wedged.
type convergence struct {
	base, ceiling time.Duration
	streak        int                 // consecutive failed attempts since the last progress; drives the backoff exponent
	retries       int                 // total failed attempts, for the user-facing count and TimeoutError
	prev          map[string]struct{} // the previous attempt's failing-resource keys, for progress detection
	lastReason    string              // the most recent failure summary, for TimeoutError
}

func newConvergence(base, ceiling time.Duration) *convergence {
	return &convergence{base: base, ceiling: ceiling}
}

// fail records a failed attempt over the given failing-resource keys and
// returns the backoff before the next attempt. The backoff resets to base when
// the failing set strictly shrinks (progress), else doubles up to cap.
func (c *convergence) fail(keys []string, reason string) time.Duration {
	set := keySet(keys)
	if c.retries > 0 && strictSubset(set, c.prev) {
		c.streak = 1
	} else {
		c.streak++
	}
	c.prev = set
	c.retries++
	if reason != "" {
		c.lastReason = reason
	}
	return backoffDelay(c.base, c.ceiling, c.streak)
}

// backoffDelay is base*2^(streak-1) capped at ceiling (streak is 1-based).
func backoffDelay(base, ceiling time.Duration, streak int) time.Duration {
	d := base
	for i := 1; i < streak; i++ {
		d *= 2
		if d >= ceiling {
			return ceiling
		}
	}
	if d > ceiling {
		return ceiling
	}
	return d
}

func keySet(keys []string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

// strictSubset reports whether a is a strict subset of b: every key in a is in
// b, and a has fewer keys. That is what "the failing set shrank" means — a
// resource that was failing stopped failing and no new one started.
func strictSubset(a, b map[string]struct{}) bool {
	if len(a) >= len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// failedResult reports whether a carried result is a task that failed — the "is
// it failing" predicate shared by the retry reason, the backoff progress set, and
// operationFailed, so all three agree. A failure shows up two ways: a completed-
// but-unsuccessful HookPhase (an apply failure is HookPhase Error; a failed hook
// is HookPhase Failed, whose Status is nonetheless Synced — so keying off
// HookPhase, not Status, covers a failed PostSync hook), or a SyncFailed status
// with an empty HookPhase (a dry-run/permission-validator failure). This is
// deliberately broader than the strip predicate below: a resource can be failing
// (must be reported) yet re-run without being stripped.
func failedResult(r common.ResourceSyncResult) bool {
	return r.Status == common.ResultCodeSyncFailed || (r.HookPhase.Completed() && !r.HookPhase.Successful())
}

// stripFailedResults returns the carried results with every completed-but-
// unsuccessful entry removed, so the next NewSyncContext re-attempts exactly
// those tasks. Its predicate is narrower than failedResult by design: a carried
// result seeds the task's operation state from its HookPhase, and only a
// *completed*-unsuccessful task (HookPhase Error or Failed) short-circuits the
// whole operation back to Failed without re-running (gitops-engine
// sync_context.go) — that is what must be dropped to re-run. A SyncFailed result
// with an empty HookPhase re-runs on its own (operationState "" ⇒ pending), so it
// need not be stripped. Succeeded resources and completed hooks (HookPhase
// Succeeded) are kept, so they are not re-run. This covers both an apply failure
// (HookPhase Error) and a failed hook (HookPhase Failed, whose Status is Synced).
func stripFailedResults(results []common.ResourceSyncResult) []common.ResourceSyncResult {
	var out []common.ResourceSyncResult
	for _, r := range results {
		if r.HookPhase.Completed() && !r.HookPhase.Successful() {
			continue
		}
		out = append(out, r)
	}
	return out
}

// stripResultKeys returns the carried results without the entries for the named
// resource keys, so they are re-attempted even though they were recorded as
// succeeded — the health gate uses it to re-apply a target that reported
// applied yet is still Missing (a CR applied against a stale REST mapping before
// its CRD registered).
func stripResultKeys(results []common.ResourceSyncResult, keys []string) []common.ResourceSyncResult {
	if len(keys) == 0 {
		return results
	}
	drop := keySet(keys)
	var out []common.ResourceSyncResult
	for _, r := range results {
		if _, ok := drop[r.ResourceKey.String()]; ok {
			continue
		}
		out = append(out, r)
	}
	return out
}

// operationFailed reports whether a completed sync operation ended in failure —
// either the operation phase is Error/Failed or a task result is SyncFailed
// (gitops-engine surfaces per-task apply failures through the result, not the
// operation error).
func operationFailed(phase common.OperationPhase, results []common.ResourceSyncResult) bool {
	if phase == common.OperationError || phase == common.OperationFailed {
		return true
	}
	for _, r := range results {
		if failedResult(r) {
			return true
		}
	}
	return false
}

// failureReason is the representative summary of a failed attempt: the
// per-resource "Kind/name: message" lines when any task failed (the actionable
// detail), else the operation phase message. Joined on "; " to stay one logical
// line — grep-able and untruncated — for the retry event and TimeoutError.
func failureReason(message string, results []common.ResourceSyncResult) string {
	var lines []string
	for _, r := range results {
		if failedResult(r) {
			lines = append(lines, fmt.Sprintf("%s: %s", r.ResourceKey.String(), r.Message))
		}
	}
	if len(lines) > 0 {
		return strings.Join(lines, "; ")
	}
	return message
}

// failedResultKeys is the set of resource keys of the failed tasks, for the
// convergence progress check (did the failing set shrink between attempts?).
func failedResultKeys(results []common.ResourceSyncResult) []string {
	var keys []string
	for _, r := range results {
		if failedResult(r) {
			keys = append(keys, r.ResourceKey.String())
		}
	}
	return keys
}

// hasMissing reports whether any pending resource is Missing — meant to be
// applied but not yet observed live.
func hasMissing(pending []ResourceStatus) bool {
	for _, p := range pending {
		if p.Status == "Missing" {
			return true
		}
	}
	return false
}

// missingKeys is the resource keys of the pending resources that are Missing,
// so the health gate can strip their results and re-apply exactly them.
func missingKeys(pending []ResourceStatus) []string {
	var keys []string
	for _, p := range pending {
		if p.Status == "Missing" {
			k := p.key()
			keys = append(keys, k.String())
		}
	}
	return keys
}

// missingReapply paces the health gate's re-apply of a target still Missing
// after a successful apply. A poll budget starts at missingReapplyPolls and
// backs off (doubling, clamped to missingReapplyMax) after each re-apply, so a
// resource merely not yet observed by the cache on the clean path is not
// re-applied, and a genuinely wedged one does not re-apply on a hot loop.
type missingReapply struct {
	polls, budget int
}

func newMissingReapply() *missingReapply {
	return &missingReapply{budget: missingReapplyPolls}
}

// due records one Missing poll and reports whether the target has stayed Missing
// long enough to re-apply. On a due poll it resets the count and backs the budget
// off (doubling, clamped to missingReapplyMax so the interval never exceeds it).
func (m *missingReapply) due() bool {
	m.polls++
	if m.polls < m.budget {
		return false
	}
	m.polls = 0
	m.budget = min(m.budget*2, missingReapplyMax)
	return true
}

// reset returns the pacing to its initial budget, called when the target is no
// longer Missing so a later Missing starts the count fresh.
func (m *missingReapply) reset() {
	m.polls, m.budget = 0, missingReapplyPolls
}

// interruptibleWait blocks for d or until ctx ends. On ctx end it returns the
// caller's mapped error (a *TimeoutError on deadline, the cancellation
// otherwise), so a backoff never swallows a Ctrl-C or a sibling-app abort.
func interruptibleWait(ctx context.Context, d time.Duration, onDone func() error) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return onDone()
	case <-t.C:
		return nil
	}
}

// retryNoun is "retry" for one, "retries" otherwise — for human-readable counts.
func retryNoun(n int) string {
	if n == 1 {
		return "retry"
	}
	return "retries"
}
