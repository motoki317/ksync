package engine

import (
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func hookPod(ns, name, phase string) *unstructured.Unstructured {
	p := podObj(ns, name, phase, "")
	p.SetAnnotations(map[string]string{"argocd.argoproj.io/hook": "PostSync"})
	return p
}

func configMap(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	}}
}

// The health gate holds a sync open until every non-hook target resource is
// observed Healthy: a still-Progressing workload and one the cache has not yet
// seen both keep it pending, while a healthy workload and a no-health resource
// clear it. This is what makes a `needs` edge mean "the dependency is serving",
// not merely "applied".
func TestPendingLines_GatesOnTargetHealthAndPresence(t *testing.T) {
	healthy := podObj("api-b", "ready-1", "Succeeded", "") // Succeeded → Healthy
	progressing := podObj("api-b", "starting-2", "Pending", "")
	settings := configMap("api-b", "settings") // no health check
	missing := podObj("api-b", "not-observed-3", "Succeeded", "")

	// lives omits `missing` entirely — the just-applied, not-yet-watched case.
	lives := map[kube.ResourceKey]*unstructured.Unstructured{
		kube.GetResourceKey(healthy):     healthy,
		kube.GetResourceKey(progressing): progressing,
		kube.GetResourceKey(settings):    settings,
	}
	target := []*unstructured.Unstructured{healthy, progressing, settings, missing}

	got := pendingLines(target, lives)
	if len(got) != 2 {
		t.Fatalf("pendingLines = %v, want exactly the Progressing and the not-yet-created", got)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "starting-2") || !strings.Contains(joined, "Progressing") {
		t.Errorf("pending = %v, want the Progressing pod named", got)
	}
	if !strings.Contains(joined, "not-observed-3") || !strings.Contains(joined, "not yet created") {
		t.Errorf("pending = %v, want the not-yet-observed target named", got)
	}
}

// A hook resource is removed by its delete policy once it runs, so it is
// legitimately absent from the cache; the gate must never wait on it.
func TestPendingLines_ExcludesHooks(t *testing.T) {
	hook := hookPod("api-b", "postsync-job", "Succeeded")
	// The hook is NOT in lives (deleted after running) and the target carries
	// only the hook — an empty result proves it does not hold the gate open.
	got := pendingLines([]*unstructured.Unstructured{hook}, map[kube.ResourceKey]*unstructured.Unstructured{})
	if len(got) != 0 {
		t.Errorf("pendingLines = %v, want none (hooks are excluded from the gate)", got)
	}
}

// Convergence: every non-hook target present and Healthy → no pending lines, so
// the gate returns and the sync completes.
func TestPendingLines_EmptyWhenAllHealthy(t *testing.T) {
	a := podObj("api-b", "ready-a", "Succeeded", "")
	b := configMap("api-b", "config-b")
	lives := map[kube.ResourceKey]*unstructured.Unstructured{
		kube.GetResourceKey(a): a,
		kube.GetResourceKey(b): b,
	}
	if got := pendingLines([]*unstructured.Unstructured{a, b}, lives); len(got) != 0 {
		t.Errorf("pendingLines = %v, want none (all healthy/no-health)", got)
	}
}
