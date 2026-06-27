package engine

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The diagnostics block lists each unhealthy resource with its events, then each
// related pod with its container state, warning events, and the one chosen log
// tail under its label.
func TestFormatDiagnostics(t *testing.T) {
	resources := []resourceDiag{{
		status: ResourceStatus{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "api", Status: "Progressing", Message: "deadline exceeded"},
		events: []string{"Warning ProgressDeadlineExceeded: ReplicaSet has timed out"},
	}}
	pods := []podDiag{{
		namespace:  "shop",
		name:       "api-7d8f",
		phase:      "Running",
		containers: []string{"api: Running (not ready, restarts 3)"},
		events:     []string{"Warning Unhealthy: Readiness probe failed: connection refused"},
		logLabel:   "logs (previous run)",
		logs:       "panic: nil pointer",
	}}

	got := strings.Join(formatDiagnostics(resources, pods, 0, 0), "\n")

	for _, want := range []string{
		"apps/Deployment/shop/api: Progressing — deadline exceeded",
		"Warning ProgressDeadlineExceeded",
		"pod shop/api-7d8f: Running",
		"api: Running (not ready, restarts 3)",
		"Warning Unhealthy: Readiness probe failed",
		"logs (previous run):",
		"panic: nil pointer",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostics missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// An intermediate controller (a ReplicaSet that cannot create its pods) is shown
// with only its identity and its warning events — it has no health verdict of its
// own, so its line carries no "Status — message", just "group/Kind/ns/name".
func TestFormatDiagnostics_ControllerEntry(t *testing.T) {
	resources := []resourceDiag{
		{status: ResourceStatus{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "api", Status: "Progressing", Message: "waiting for rollout"}},
		{
			status: statusFromKey(ResourceStatus{Group: "apps", Kind: "ReplicaSet", Namespace: "shop", Name: "api-5dd4"}.key()),
			events: []string{`Warning FailedCreate: pods "api-5dd4-x" is forbidden: exceeded quota: block-pods`},
		},
	}
	got := strings.Join(formatDiagnostics(resources, nil, 0, 0), "\n")
	if !strings.Contains(got, "apps/ReplicaSet/shop/api-5dd4\n") {
		t.Errorf("controller line should be bare identity (no trailing status), got:\n%s", got)
	}
	if strings.Contains(got, "api-5dd4:") {
		t.Errorf("controller with no health status should not render a trailing colon, got:\n%s", got)
	}
	if !strings.Contains(got, "Warning FailedCreate: pods") || !strings.Contains(got, "exceeded quota") {
		t.Errorf("controller warning event should show, got:\n%s", got)
	}
}

// A pod with no captured logs shows no "logs:" header — the dump stays scannable.
func TestFormatDiagnostics_OmitsEmptyLogs(t *testing.T) {
	pods := []podDiag{{namespace: "team-a", name: "worker-0", phase: "Pending", containers: []string{"worker: Waiting (ImagePullBackOff)"}}}
	got := strings.Join(formatDiagnostics(nil, pods, 0, 0), "\n")
	if strings.Contains(got, "logs") {
		t.Errorf("empty logs should be omitted, got:\n%s", got)
	}
	if !strings.Contains(got, "worker: Waiting (ImagePullBackOff)") {
		t.Errorf("container state should still show, got:\n%s", got)
	}
}

// Identical replica failures collapse to one representative carrying a count, so
// three CrashLooping replicas show one log tail, not three.
func TestFormatDiagnostics_CollapsesIdenticalPods(t *testing.T) {
	pods := []podDiag{{
		namespace:  "shop",
		name:       "api-abc",
		phase:      "Running",
		containers: []string{"app: Terminated (Error, exit 1)"},
		events:     []string{"Warning BackOff: Back-off restarting failed container"},
		logLabel:   "logs",
		logs:       "FATAL: boom",
		dupes:      2,
	}}
	got := strings.Join(formatDiagnostics(nil, pods, 0, 0), "\n")
	if !strings.Contains(got, "pod shop/api-abc (+2 more identical): Running") {
		t.Errorf("expected collapsed count in header, got:\n%s", got)
	}
}

// When more distinct failure modes exist than the per-run pod cap, the dump notes
// the omitted count rather than silently implying it showed everything; likewise
// when the pod scan was bounded, it names how many related pods went uninspected.
func TestFormatDiagnostics_NotesOmittedAndUninspected(t *testing.T) {
	pods := []podDiag{{namespace: "shop", name: "api-1", phase: "Running", containers: []string{"app: Running (not ready, restarts 0)"}}}
	got := strings.Join(formatDiagnostics(nil, pods, 3, 0), "\n")
	if !strings.Contains(got, "(+3 more distinct failure modes not shown)") {
		t.Errorf("expected omitted-modes note, got:\n%s", got)
	}
	single := strings.Join(formatDiagnostics(nil, pods, 1, 0), "\n")
	if !strings.Contains(single, "(+1 more distinct failure mode not shown)") {
		t.Errorf("expected singular omitted-mode note, got:\n%s", single)
	}
	scanned := strings.Join(formatDiagnostics(nil, pods, 0, 26), "\n")
	if !strings.Contains(scanned, "(+26 related pods not inspected)") {
		t.Errorf("expected uninspected-pods note, got:\n%s", scanned)
	}
}

// dedupePods groups pods whose only difference is the pod name, restart count, or
// event repeat count — a Deployment's replicas — but keeps genuinely different
// failures (a different exit reason) apart.
func TestDedupePods(t *testing.T) {
	mk := func(name, state, ev string) podDiag {
		return podDiag{namespace: "ns", name: name, containers: []string{state}, events: []string{ev}}
	}
	pods := []podDiag{
		mk("api-1", "app: Terminated (Error, exit 1)", "Warning BackOff: restarting api-1 (x3)"),
		mk("api-2", "app: Terminated (Error, exit 1)", "Warning BackOff: restarting api-2 (x5)"),
		mk("api-3", "app: Terminated (OOMKilled, exit 137)", "Warning BackOff: restarting api-3 (x2)"),
	}
	got := dedupePods(pods)
	if len(got) != 2 {
		t.Fatalf("want 2 distinct groups (exit 1 vs OOMKilled), got %d:\n%+v", len(got), got)
	}
	if got[0].dupes != 1 {
		t.Errorf("the two exit-1 replicas should collapse to one with dupes=1, got dupes=%d", got[0].dupes)
	}
	if got[1].dupes != 0 {
		t.Errorf("the OOMKilled pod is distinct, want dupes=0, got %d", got[1].dupes)
	}
}

// displayEvents drops Normal events (Scheduled, Pulled, Started — noise) and the
// redundant BackOff warning (the container state already shows the back-off),
// keeping only the actionable warnings.
func TestDisplayEvents_WarningsOnly(t *testing.T) {
	evs := []eventInfo{
		{warning: false, reason: "Scheduled", message: "assigned to node", count: 1, at: at(1)},
		{warning: false, reason: "Pulled", message: "image present", count: 1, at: at(2)},
		{warning: true, reason: "BackOff", message: "Back-off restarting failed container", count: 9, at: at(3)},
		{warning: true, reason: "Unhealthy", message: "Liveness probe failed: connection refused", count: 4, at: at(4)},
	}
	got := strings.Join(displayEvents(evs, true), "\n")
	for _, drop := range []string{"Scheduled", "Pulled", "BackOff"} {
		if strings.Contains(got, drop) {
			t.Errorf("%s should be dropped, got:\n%s", drop, got)
		}
	}
	if !strings.Contains(got, "Warning Unhealthy: Liveness probe failed: connection refused (x4)") {
		t.Errorf("actionable warning with count missing, got:\n%s", got)
	}
}

// With no warnings, the single latest event surfaces only when nothing else
// carries the signal (no container state, no logs) — a pod stuck with only a
// Normal event still says something; one that already shows its state stays terse.
func TestDisplayEvents_NormalFallback(t *testing.T) {
	evs := []eventInfo{{warning: false, reason: "Pulling", message: "pulling image", count: 1, at: at(1)}}
	if got := displayEvents(evs, false); len(got) != 1 || !strings.Contains(got[0], "Pulling") {
		t.Errorf("with no other signal, latest event should show, got: %v", got)
	}
	if got := displayEvents(evs, true); got != nil {
		t.Errorf("with a signal present, Normal events stay hidden, got: %v", got)
	}
}

// An ImagePullBackOff fires three same-reason events restating each other;
// dedupeEvents keeps only the most informative one.
func TestDedupeEvents_KeepsMostInformativePerReason(t *testing.T) {
	evs := []eventInfo{
		{warning: true, reason: "Failed", message: "Error: ImagePullBackOff", count: 2, at: at(1)},
		{warning: true, reason: "Failed", message: "Error: ErrImagePull", count: 2, at: at(2)},
		{warning: true, reason: "Failed", message: `Failed to pull image "x": no such host`, count: 2, at: at(3)},
	}
	got := displayEvents(evs, true)
	if len(got) != 1 {
		t.Fatalf("three same-reason events should collapse to one, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "no such host") {
		t.Errorf("the most informative message should win, got: %s", got[0])
	}
}

// containerState renders the reason that explains a stuck pod, and for a
// CrashLoopBackOff appends the last-termination cause — the real exit code or
// OOMKilled lives there, not in the bland "CrashLoopBackOff".
func TestContainerState(t *testing.T) {
	cases := []struct {
		name string
		cs   corev1.ContainerStatus
		want string
	}{
		{"waiting", corev1.ContainerStatus{Name: "api", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}, "api: Waiting (ImagePullBackOff)"},
		{"crashloop with last termination", corev1.ContainerStatus{
			Name:                 "api",
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		}, "api: Waiting (CrashLoopBackOff); last: Terminated (OOMKilled, exit 137)"},
		{"terminated", corev1.ContainerStatus{Name: "job", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}, "job: Terminated (Error, exit 1)"},
		{"running ready", corev1.ContainerStatus{Name: "db", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, "db: Running (ready, restarts 0)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := containerState(c.cs); got != c.want {
				t.Errorf("containerState = %q, want %q", got, c.want)
			}
		})
	}
}

// TimeoutError names every resource still not healthy, so the message alone
// (before any diagnostic dump) tells the developer what the sync waited on.
func TestTimeoutError_NamesPending(t *testing.T) {
	err := &TimeoutError{App: "shop", Pending: []ResourceStatus{
		{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "api", Status: "Progressing"},
		{Kind: "Pod", Namespace: "shop", Name: "migrate", Status: "Missing", Message: "not yet created"},
	}}
	msg := err.Error()
	for _, want := range []string{"shop", "timed out", "api", "migrate", "not yet created"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func at(s int) time.Time { return time.Unix(int64(s), 0) }
