// Package engine wraps gitops-engine (the library behind ArgoCD's sync)
// behind a small surface so its v0.x API churn stays localized here. The
// wrapper owns the warm cluster cache — the core performance advantage of a
// long-running ksync process — and the tracking-label contract that scopes
// prune. Pinning and safety rationale:
// docs/ADR/20260612-gitops-engine-and-prune-safety.md.
package engine

import (
	"fmt"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/cache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/engine"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// TrackingLabel marks every resource ksync applies; its value is the app
	// name. The isManaged predicate (and therefore prune) only ever matches
	// resources carrying the right value, so ksync cannot delete anything it
	// did not create.
	TrackingLabel = "ksync.dev/app"
	// fieldManager identifies ksync as the server-side-apply field owner.
	fieldManager = "ksync"
)

// resourceInfo is attached to every cached cluster resource; it memoizes the
// tracking-label value so isManaged checks need no live object access.
type resourceInfo struct {
	app string
}

// Engine is a connected sync engine with a running cluster cache.
type Engine struct {
	clusterCache cache.ClusterCache
	engine       engine.GitOpsEngine
	stop         engine.StopFunc
	log          logr.Logger
}

// New connects to the cluster behind the named kubectl context and starts
// the cluster cache (watches stay open until Close — never spawn one Engine
// per sync). The context name is required; ksync never falls back to the
// kubeconfig's current-context.
func New(kubeContext string, log logr.Logger) (*Engine, error) {
	cfg, err := RESTConfig(kubeContext)
	if err != nil {
		return nil, err
	}
	clusterCache := cache.NewClusterCache(cfg,
		cache.SetLogr(log),
		cache.SetPopulateResourceInfoHandler(func(un *unstructured.Unstructured, _ bool) (any, bool) {
			app := un.GetLabels()[TrackingLabel]
			// Cache full manifests only for ksync-managed resources: fast
			// diffs for what we sync, no memory cost for the rest of the
			// cluster.
			return &resourceInfo{app: app}, app != ""
		}),
	)
	gitopsEngine := engine.NewEngine(cfg, clusterCache, engine.WithLogr(log))
	stop, err := gitopsEngine.Run()
	if err != nil {
		return nil, fmt.Errorf("starting gitops engine: %w", err)
	}
	return &Engine{clusterCache: clusterCache, engine: gitopsEngine, stop: stop, log: log}, nil
}

// Close stops the cluster cache watches.
func (e *Engine) Close() {
	e.stop()
}
