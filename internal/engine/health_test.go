package engine

import (
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/health"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// eckES builds an ECK Elasticsearch live object with the given desired node
// counts (one per nodeSet) and status fields. A nil status arg omits .status.
func eckES(nodeCounts []int64, status map[string]interface{}) *unstructured.Unstructured {
	nodeSets := make([]interface{}, len(nodeCounts))
	for i, c := range nodeCounts {
		nodeSets[i] = map[string]interface{}{"count": c}
	}
	obj := map[string]interface{}{
		"apiVersion": "elasticsearch.k8s.elastic.co/v1",
		"kind":       "Elasticsearch",
		"metadata":   map[string]interface{}{"name": "es", "namespace": "ns"},
		"spec":       map[string]interface{}{"nodeSets": nodeSets},
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestEckElasticsearchHealth(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want health.HealthStatusCode
	}{
		{"no status yet", eckES([]int64{3}, nil), health.HealthStatusUnknown},
		{"availableNodes absent", eckES([]int64{3}, map[string]interface{}{"phase": "Ready"}), health.HealthStatusUnknown},
		{"fewer nodes than desired", eckES([]int64{2, 1}, map[string]interface{}{"availableNodes": int64(1)}), health.HealthStatusProgressing},
		{"ready green", eckES([]int64{3}, map[string]interface{}{"availableNodes": int64(3), "phase": "Ready", "health": "green"}), health.HealthStatusHealthy},
		{"ready yellow", eckES([]int64{3}, map[string]interface{}{"availableNodes": int64(3), "phase": "Ready", "health": "yellow"}), health.HealthStatusProgressing},
		{"ready red", eckES([]int64{3}, map[string]interface{}{"availableNodes": int64(3), "phase": "Ready", "health": "red"}), health.HealthStatusDegraded},
		{"applying changes", eckES([]int64{3}, map[string]interface{}{"availableNodes": int64(3), "phase": "ApplyingChanges", "health": "green"}), health.HealthStatusProgressing},
		{"migrating data", eckES([]int64{3}, map[string]interface{}{"availableNodes": int64(3), "phase": "MigratingData", "health": "green"}), health.HealthStatusProgressing},
		{"invalid", eckES([]int64{3}, map[string]interface{}{"availableNodes": int64(3), "phase": "Invalid", "health": "green"}), health.HealthStatusDegraded},
		// availableNodes meets desired but colour is absent → upstream falls to Unknown.
		{"ready no colour", eckES([]int64{3}, map[string]interface{}{"availableNodes": int64(3), "phase": "Ready"}), health.HealthStatusUnknown},
		// Scaling down can momentarily over-shoot the desired count; upstream treats
		// only exact equality as a valid state, anything else as Unknown.
		{"more nodes than desired", eckES([]int64{1}, map[string]interface{}{"availableNodes": int64(2), "phase": "Ready", "health": "green"}), health.HealthStatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := eckElasticsearchHealth(tc.obj)
			if got == nil {
				t.Fatalf("got nil health status, want %s", tc.want)
			}
			if got.Status != tc.want {
				t.Errorf("status = %s, want %s (message: %q)", got.Status, tc.want, got.Message)
			}
		})
	}
}

// A green ECK ES must be Healthy so a sync wave advances; anything short of green
// (Progressing/Unknown/Degraded) must NOT be Healthy so the wave holds. This is
// the property the wave gate depends on.
func TestEckElasticsearchHealthGatesUntilGreen(t *testing.T) {
	green := eckES([]int64{3}, map[string]interface{}{"availableNodes": int64(3), "phase": "Ready", "health": "green"})
	if eckElasticsearchHealth(green).Status != health.HealthStatusHealthy {
		t.Fatal("green cluster must be Healthy")
	}
	notReady := eckES([]int64{3}, map[string]interface{}{"availableNodes": int64(1)})
	if eckElasticsearchHealth(notReady).Status == health.HealthStatusHealthy {
		t.Fatal("a still-converging cluster must not read Healthy — the wave would not wait")
	}
}

// The override returns (nil, nil) for kinds it does not cover, so they fall
// through to gitops-engine's built-in checks unchanged.
func TestResourceHealthFallsThroughForUnknownKind(t *testing.T) {
	cm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "c", "namespace": "ns"},
	}}
	h, err := resourceHealth.GetResourceHealth(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h != nil {
		t.Fatalf("want nil health (fall through to built-in), got %v", h)
	}
}
