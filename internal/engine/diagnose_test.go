package engine

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The diagnostics block lists each unhealthy resource with its events, then each
// related pod with its container state and current/previous log tails — every
// log line indented under its labelled block, and an absent previous log omitted.
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
		events:     []string{"Warning BackOff: Back-off restarting failed container"},
		logs:       "starting up\nlistening on :8080",
		prevLogs:   "panic: nil pointer",
	}}

	got := strings.Join(formatDiagnostics(resources, pods), "\n")

	for _, want := range []string{
		"apps/Deployment/shop/api: Progressing — deadline exceeded",
		"Warning ProgressDeadlineExceeded",
		"pod shop/api-7d8f: Running",
		"api: Running (not ready, restarts 3)",
		"Warning BackOff: Back-off restarting failed container",
		"logs:",
		"listening on :8080",
		"logs (previous):",
		"panic: nil pointer",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostics missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// A pod with no captured logs shows neither an empty "logs:" header nor a
// previous block — the dump stays scannable.
func TestFormatDiagnostics_OmitsEmptyLogs(t *testing.T) {
	pods := []podDiag{{namespace: "team-a", name: "worker-0", phase: "Pending", containers: []string{"worker: Waiting (ImagePullBackOff)"}}}
	got := strings.Join(formatDiagnostics(nil, pods), "\n")
	if strings.Contains(got, "logs") {
		t.Errorf("empty logs should be omitted, got:\n%s", got)
	}
	if !strings.Contains(got, "worker: Waiting (ImagePullBackOff)") {
		t.Errorf("container state should still show, got:\n%s", got)
	}
}

// A pod that never ran a container (unschedulable, failed mount) has no
// container state and no logs — its events are the only signal, so the dump
// must surface them.
func TestFormatDiagnostics_PendingPodEventsSurface(t *testing.T) {
	pods := []podDiag{{
		namespace: "team-a",
		name:      "db-0",
		phase:     "Pending",
		events:    []string{"Warning FailedScheduling: 0/3 nodes are available: insufficient memory"},
	}}
	got := strings.Join(formatDiagnostics(nil, pods), "\n")
	if !strings.Contains(got, "Warning FailedScheduling") {
		t.Errorf("pending pod events should surface, got:\n%s", got)
	}
}

// containerState renders the reason that explains a stuck pod — the waiting or
// terminated cause is the whole story (ImagePull, CrashLoop, OOMKilled).
func TestContainerState(t *testing.T) {
	cases := []struct {
		name string
		cs   corev1.ContainerStatus
		want string
	}{
		{"waiting", corev1.ContainerStatus{Name: "api", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}, "api: Waiting (ImagePullBackOff)"},
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
