package main

import (
	"fmt"
	"strings"

	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/ui"
)

// deployWait reports the health gate's progress on the app's deploy row: each
// poll that is still waiting updates the not-ready count and names the first few
// resources. An already-healthy sync never calls back, so the row simply
// finishes with its elapsed time.
func deployWait(deploy *ui.Stage) func([]engine.ResourceStatus) {
	return func(pending []engine.ResourceStatus) {
		deploy.SetTail(waitingTail(pending))
	}
}

// waitingTail renders the live health-gate line: the not-ready count plus the
// first few resources by short name (with a "+N" overflow), so the developer
// sees WHAT the deploy is waiting on without the line growing unbounded.
func waitingTail(pending []engine.ResourceStatus) string {
	const show = 3
	tail := fmt.Sprintf("waiting for health  %d not ready", len(pending))
	if len(pending) == 0 {
		return tail
	}
	names := make([]string, 0, show)
	for i, p := range pending {
		if i >= show {
			break
		}
		names = append(names, p.ShortName())
	}
	tail += ": " + strings.Join(names, ", ")
	if len(pending) > show {
		tail += fmt.Sprintf(", +%d", len(pending)-show)
	}
	return tail
}

// deployRetry reports the sync's convergence retries on the app's deploy row and
// in scrollback: a failing apply is retried until it succeeds or --timeout, and
// a first-time user watching either output must know what failed, what ksync is
// doing about it, and when. Each attempt updates the live row (the transient
// countdown), and each *distinct* failure — plus the eventual recovery — commits
// one persistent line, so a slow failure yields a few lines, not one per attempt.
// The retry total is recorded so the committed deploy line can show "(N retries)".
func deployRetry(deploy *ui.Stage, c ui.Colors, app string, prog *progress) func(engine.RetryEvent) {
	var lastReason string
	return func(ev engine.RetryEvent) {
		prog.recordRetries(app, ev.Retries)
		if ev.Recovered {
			deploy.SetTailQuiet("") // drop the countdown; a following health wait sets its own tail
			deploy.Event(c.Green("✓"), retryLine(ev))
			lastReason = ""
			return
		}
		deploy.SetTailQuiet(retryTail(ev))
		if ev.Reason != lastReason {
			lastReason = ev.Reason
			deploy.Event(c.Yellow("⚠"), retryLine(ev))
		}
	}
}

// retryLine is the committed failure/recovery text for a convergence retry, in
// first-time-user language — it names what ksync is doing, not the internals.
func retryLine(ev engine.RetryEvent) string {
	if ev.Recovered {
		return fmt.Sprintf("apply recovered after %d %s", ev.Retries, retryNoun(ev.Retries))
	}
	return fmt.Sprintf("apply failed (attempt %d, retrying in %s): %s", ev.Attempt, ev.NextDelay, ev.Reason)
}

// retryTail is the transient live-row countdown shown on a terminal while a retry
// backs off.
func retryTail(ev engine.RetryEvent) string {
	return fmt.Sprintf("retrying after apply failure (attempt %d, next in %s)", ev.Attempt, ev.NextDelay)
}

// withRetryCount appends "(N retries)" to a deploy summary when the sync retried,
// so the committed line and the Timings recap show it took work to converge.
func withRetryCount(summary string, retries int) string {
	if retries <= 0 {
		return summary
	}
	suffix := fmt.Sprintf("(%d %s)", retries, retryNoun(retries))
	if summary == "" {
		return suffix
	}
	return summary + "  " + suffix
}

// retryNoun is "retry" for one, "retries" otherwise.
func retryNoun(n int) string {
	if n == 1 {
		return "retry"
	}
	return "retries"
}
