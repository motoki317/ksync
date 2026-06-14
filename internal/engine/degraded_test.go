package engine

import (
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func podObj(ns, name, phase, msg string) *unstructured.Unstructured {
	status := map[string]any{"phase": phase}
	if msg != "" {
		status["message"] = msg
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"status":     status,
	}}
}

// degradedLines reports only resources that are genuinely broken (Degraded), so
// a post-sync snapshot never flags a rollout that is merely Progressing.
func TestDegradedLines_FlagsDegradedNotProgressing(t *testing.T) {
	failed := podObj("api-b", "broken-7", "Failed", "OOMKilled")
	pending := podObj("api-b", "rolling-9", "Pending", "")
	noHealth := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "settings", "namespace": "api-b"},
	}}

	lives := map[kube.ResourceKey]*unstructured.Unstructured{
		kube.GetResourceKey(failed):   failed,
		kube.GetResourceKey(pending):  pending,
		kube.GetResourceKey(noHealth): noHealth,
	}

	got := degradedLines(lives)
	if len(got) != 1 {
		t.Fatalf("degradedLines = %v, want exactly the one Degraded pod", got)
	}
	if !strings.Contains(got[0], "broken-7") || !strings.Contains(got[0], "Degraded") {
		t.Errorf("line = %q, want it to name broken-7 as Degraded", got[0])
	}
	if !strings.Contains(got[0], "OOMKilled") {
		t.Errorf("line = %q, want the health message appended", got[0])
	}
}

func TestDegradedLines_EmptyWhenNothingBroken(t *testing.T) {
	pending := podObj("api-b", "rolling-9", "Pending", "")
	lives := map[kube.ResourceKey]*unstructured.Unstructured{
		kube.GetResourceKey(pending): pending,
	}
	if got := degradedLines(lives); len(got) != 0 {
		t.Errorf("degradedLines = %v, want none (Progressing is not Degraded)", got)
	}
}
