package engine

import (
	"fmt"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/health"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// resourceHealth is the health.HealthOverride ksync passes everywhere it reads
// resource health — gitops-engine's sync-wave gate (sync.WithHealthOverride) and
// ksync's own post-apply gate. gitops-engine has no built-in health check for
// custom resources, so without an override a CR reads as healthy the instant it
// exists and a sync wave never waits for it. This supplies health for such CRs
// from their controller-maintained .status, the way ArgoCD's resource
// customizations do — so a later-wave consumer waits for an earlier-wave
// dependency (an ECK Elasticsearch) to actually serve.
//
// A GroupKind with no entry returns (nil, nil) and falls through to
// gitops-engine's built-in checks — the same contract ArgoCD's Lua override
// follows (an empty health script means "use the built-in").
var resourceHealth health.HealthOverride = customResourceHealth{}

// crHealthFunc assesses one custom resource's health from its live object.
type crHealthFunc func(*unstructured.Unstructured) *health.HealthStatus

// crHealthChecks maps a custom resource's GroupKind to its health assessment.
// Each entry ports the matching ArgoCD resource_customizations health.lua so
// ksync's wave ordering matches ArgoCD's; add an entry to gate a new CR kind.
var crHealthChecks = map[schema.GroupKind]crHealthFunc{
	{Group: "elasticsearch.k8s.elastic.co", Kind: "Elasticsearch"}: eckElasticsearchHealth,
}

type customResourceHealth struct{}

func (customResourceHealth) GetResourceHealth(obj *unstructured.Unstructured) (*health.HealthStatus, error) {
	if fn := crHealthChecks[obj.GroupVersionKind().GroupKind()]; fn != nil {
		return fn(obj), nil
	}
	return nil, nil
}

// eckElasticsearchHealth ports ArgoCD's
// resource_customizations/elasticsearch.k8s.elastic.co/Elasticsearch/health.lua.
// The ECK operator rolls node count and cluster colour into the Elasticsearch
// CR's .status; this reads them so the CR is Healthy only once every desired
// node is up and the cluster is green. Until then it returns Progressing or
// Unknown, which holds the sync wave — gitops-engine advances a wave only on
// Healthy, fails it on Degraded, and keeps waiting on Progressing/Unknown.
func eckElasticsearchHealth(obj *unstructured.Unstructured) *health.HealthStatus {
	unknown := &health.HealthStatus{
		Status:  health.HealthStatusUnknown,
		Message: "Elasticsearch cluster status is unknown",
	}

	availableNodes, found, err := unstructured.NestedInt64(obj.Object, "status", "availableNodes")
	if err != nil || !found {
		return unknown
	}

	// Desired size is the sum of every nodeSet's count — the spec's source of
	// truth for the cluster; availableNodes must reach it before the cluster is up.
	var desired int64
	nodeSets, _, _ := unstructured.NestedSlice(obj.Object, "spec", "nodeSets")
	for _, ns := range nodeSets {
		m, ok := ns.(map[string]interface{})
		if !ok {
			continue
		}
		if count, ok, _ := unstructured.NestedInt64(m, "count"); ok {
			desired += count
		}
	}

	if availableNodes < desired {
		return &health.HealthStatus{
			Status:  health.HealthStatusProgressing,
			Message: fmt.Sprintf("The desired amount of availableNodes is %d but the current amount is %d", desired, availableNodes),
		}
	}
	if availableNodes != desired {
		return unknown
	}

	// availableNodes == desired: classify on phase + colour, both of which the
	// upstream script requires to be present before trusting the phase.
	phase, phaseOK, _ := unstructured.NestedString(obj.Object, "status", "phase")
	colour, colourOK, _ := unstructured.NestedString(obj.Object, "status", "health")
	if !phaseOK || !colourOK {
		return unknown
	}
	switch phase {
	case "Ready":
		switch colour {
		case "green":
			return &health.HealthStatus{Status: health.HealthStatusHealthy, Message: "Elasticsearch Cluster status is Green"}
		case "yellow":
			return &health.HealthStatus{Status: health.HealthStatusProgressing, Message: "Elasticsearch Cluster status is Yellow. Check the status of indices, replicas and shards"}
		case "red":
			return &health.HealthStatus{Status: health.HealthStatusDegraded, Message: "Elasticsearch Cluster status is Red. Check the status of indices, replicas and shards"}
		}
	case "ApplyingChanges":
		return &health.HealthStatus{Status: health.HealthStatusProgressing, Message: "Elasticsearch phase is ApplyingChanges"}
	case "MigratingData":
		return &health.HealthStatus{Status: health.HealthStatusProgressing, Message: "Elasticsearch phase is MigratingData"}
	case "Invalid":
		return &health.HealthStatus{Status: health.HealthStatusDegraded, Message: "Elasticsearch phase is Invalid"}
	}
	return unknown
}
