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
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/tracing"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

// appManaged reports whether a cached resource is a prune-eligible managed
// resource of app — the single predicate every prune/diff/health path keys on.
// Namespaces are deliberately excluded: an authored Namespace is still applied
// (target objects are applied regardless of this predicate), but it is never a
// prune candidate, so neither a prune nor a `ksync destroy` deletes a namespace
// and cascades via Kubernetes GC into another app's resources sharing it. This
// also neutralizes a Namespace a prior ksync version labeled — prune candidacy is
// decided here, from the live cache, not from what the current run stamps. It
// mirrors how auto-created and referenced namespaces are already left bare.
func appManaged(app string) func(*cache.Resource) bool {
	return func(r *cache.Resource) bool {
		info, ok := r.Info.(*resourceInfo)
		if !ok || info.app != app {
			return false
		}
		return !isNamespace(r.ResourceKey())
	}
}

// isNamespace reports whether a resource key is a core/v1 Namespace.
func isNamespace(k kube.ResourceKey) bool {
	return k.Group == "" && k.Kind == "Namespace"
}

// Engine is a connected sync engine with a running cluster cache.
type Engine struct {
	clusterCache cache.ClusterCache
	stop         engine.StopFunc
	// cfg and kubectl let Sync drive gitops-engine's pkg/sync directly, in a
	// re-reconciling loop, instead of the one-shot engine.GitOpsEngine.Sync
	// convenience wrapper — see Sync for why that wrapper deadlocks on hooks.
	cfg     *rest.Config
	kubectl kube.Kubectl
	// kclient creates namespaces an app's resources reference but does not own
	// (cross-namespace resources); gitops-engine's namespace modifier only
	// creates the app's own destination namespace.
	kclient kubernetes.Interface
	log     logr.Logger
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
	// Run() only warms the cache (EnsureSynced) and returns Invalidate as its
	// stop; Sync no longer goes through GitOpsEngine, but this keeps the cache
	// lifecycle and its kubectl defaults identical to the engine's.
	gitopsEngine := engine.NewEngine(cfg, clusterCache, engine.WithLogr(log))
	stop, err := gitopsEngine.Run()
	if err != nil {
		return nil, fmt.Errorf("starting gitops engine: %w", err)
	}
	kclient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	return &Engine{
		clusterCache: clusterCache,
		stop:         stop,
		cfg:          cfg,
		// The same kubectl engine.NewEngine builds by default (ctl.go), recreated
		// here because that one is not reachable through the GitOpsEngine surface.
		kubectl: &kube.KubectlCmd{Log: log, Tracer: tracing.NopTracer{}},
		kclient: kclient,
		log:     log,
	}, nil
}

// Close stops the cluster cache watches.
func (e *Engine) Close() {
	e.stop()
}
