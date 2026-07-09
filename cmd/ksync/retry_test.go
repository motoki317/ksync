package main

import (
	"strings"
	"testing"
	"time"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/ui"
)

// retryLine speaks to a first-time user: it names the attempt, when ksync will
// retry, and the resource error verbatim — and never leaks engine internals.
func TestRetryLine(t *testing.T) {
	fail := retryLine(engine.RetryEvent{Attempt: 2, NextDelay: 2 * time.Second, Reason: `Route/team-a/main: no matches for kind "Route"`})
	for _, want := range []string{"attempt 2", "retrying in 2s", "no matches for kind"} {
		if !strings.Contains(fail, want) {
			t.Errorf("failure line %q missing %q", fail, want)
		}
	}
	for _, jargon := range []string{"reconcile", "task", "phase", "syncCtx", "OperationFailed"} {
		if strings.Contains(fail, jargon) {
			t.Errorf("failure line must avoid internal jargon %q: %q", jargon, fail)
		}
	}
	if one := retryLine(engine.RetryEvent{Recovered: true, Retries: 1}); one != "apply recovered after 1 retry" {
		t.Errorf("single-retry recovery = %q", one)
	}
	if many := retryLine(engine.RetryEvent{Recovered: true, Retries: 3}); many != "apply recovered after 3 retries" {
		t.Errorf("multi-retry recovery = %q", many)
	}
}

func TestWithRetryCount(t *testing.T) {
	cases := []struct {
		summary string
		retries int
		want    string
	}{
		{"36 applied", 0, "36 applied"},
		{"36 applied", 1, "36 applied  (1 retry)"},
		{"36 applied", 3, "36 applied  (3 retries)"},
		{"", 2, "(2 retries)"},
	}
	for _, tc := range cases {
		if got := withRetryCount(tc.summary, tc.retries); got != tc.want {
			t.Errorf("withRetryCount(%q, %d) = %q, want %q", tc.summary, tc.retries, got, tc.want)
		}
	}
}

// The retry handler commits one persistent line per *distinct* failure (deduped
// by reason, so a slow failure does not spam the log), one recovery line, and
// records the retry total for the committed "(N retries)" suffix.
func TestDeployRetry_DedupesDistinctFailuresAndRecovers(t *testing.T) {
	prog, buf := offProgress([]config.App{{Name: "shop"}})
	deploy := prog.pipeline("shop").Deploy()
	deploy.Start()
	onRetry := deployRetry(deploy, ui.NewColors(buf), "shop", prog)

	reasonA := `Route/team-a/main: no matches for kind "Route"`
	reasonB := "Deployment/team-a/api: admission webhook denied"
	onRetry(engine.RetryEvent{Attempt: 1, NextDelay: time.Second, Reason: reasonA, Retries: 1})
	onRetry(engine.RetryEvent{Attempt: 2, NextDelay: 2 * time.Second, Reason: reasonA, Retries: 2}) // same reason: no new committed line
	onRetry(engine.RetryEvent{Attempt: 3, NextDelay: 4 * time.Second, Reason: reasonB, Retries: 3})
	onRetry(engine.RetryEvent{Recovered: true, Attempt: 3, Retries: 3})

	out := buf.String()
	if n := strings.Count(out, "apply failed"); n != 2 {
		t.Errorf("distinct failures should commit one line each (deduped by reason), got %d:\n%s", n, out)
	}
	if !strings.Contains(out, reasonA) || !strings.Contains(out, reasonB) {
		t.Errorf("each committed failure should carry its reason verbatim, got:\n%s", out)
	}
	if !strings.Contains(out, "apply recovered after 3 retries") {
		t.Errorf("recovery should commit a line naming the retry count, got:\n%s", out)
	}
	if got := prog.retriesFor("shop"); got != 3 {
		t.Errorf("retriesFor = %d, want 3 (for the committed \"(N retries)\" suffix)", got)
	}
}
