package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/cache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/diff"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/health"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SyncOptions tune one Sync call.
type SyncOptions struct {
	// Prune deletes tracked resources of the app that are absent from the
	// target set.
	Prune bool
	// Namespace is the app's default namespace (ArgoCD destination.namespace
	// parity): gitops-engine stamps it on target objects that set none, and
	// when non-empty the namespace itself is created on sync if missing.
	Namespace string
}

// Sync makes the cluster state of one app match the given rendered resources,
// with ArgoCD semantics: server-side apply, hook phases and waves from the
// resource annotations, health-gated PostSync, and tracking-label-scoped
// prune.
func (e *Engine) Sync(ctx context.Context, app string, resources []*unstructured.Unstructured, opts SyncOptions) ([]common.ResourceSyncResult, error) {
	target := StampTracking(app, resources)
	isManaged := func(r *cache.Resource) bool {
		info, ok := r.Info.(*resourceInfo)
		return ok && info.app == app
	}
	// Fill the default namespace before any key-based matching: the live-state
	// lookup and the diff below key resources by the target's namespace, so a
	// late fill (gitops-engine also stamps at task creation) would mismatch.
	if opts.Namespace != "" {
		fillDefaultNamespace(target, opts.Namespace, e.clusterCache.IsNamespaced)
	}

	syncOpts := []sync.SyncOpt{
		sync.WithLogr(e.log),
		sync.WithPrune(opts.Prune),
		// Production parity: the reference ArgoCD setup applies everything
		// server-side.
		sync.WithServerSideApply(true),
		sync.WithServerSideApplyManager(fieldManager),
		// Registering a namespace modifier turns on gitops-engine's namespace
		// auto-creation for opts.Namespace (a no-op when it is empty).
		sync.WithNamespaceModifier(createNamespaceIfMissing),
	}

	// Apply only resources whose desired state differs from the warm cache's
	// live state (ArgoCD's ApplyOutOfSyncOnly). On a large app this is the
	// difference between O(changed) and O(all) API round-trips per sync; a
	// no-change sync applies nothing. Skipped on error: applying everything
	// is the safe fallback.
	if lives, err := e.clusterCache.GetManagedLiveObjs(target, isManaged); err == nil {
		if diffRes, err := diff.DiffArray(target, alignedLiveObjs(target, lives)); err == nil {
			syncOpts = append(syncOpts, sync.WithResourceModificationChecker(true, diffRes))
		}
	}

	results, err := e.engine.Sync(ctx, target, isManaged, revision(target), opts.Namespace, syncOpts...)
	if err != nil {
		// gitops-engine blocks until the operation completes or ctx is done;
		// on a health-gated wave (or PostSync hook waiting on Healthy main
		// resources) a stuck workload — ErrImagePull, CrashLoop — hangs the
		// sync until the caller's timeout fires. Turn the bare deadline error
		// into the actionable "which resource is stuck" the user needs.
		if errors.Is(err, context.DeadlineExceeded) {
			return results, e.timeoutError(app, target, isManaged)
		}
		return results, err
	}
	// gitops-engine returns a nil error when the operation completed with
	// task-level failures (it errors only on operation-level errors), so a
	// failed apply would otherwise look like success — the watch loop would
	// log "synced" and never retry.
	return results, failedResultsError(results)
}

// failedResultsError condenses task-level failures into one error, or nil if
// every task succeeded.
func failedResultsError(results []common.ResourceSyncResult) error {
	var failed []string
	for _, res := range results {
		if res.Status == common.ResultCodeSyncFailed {
			failed = append(failed, fmt.Sprintf("%s: %s", res.ResourceKey.String(), res.Message))
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d resource(s) failed to sync:\n%s", len(failed), strings.Join(failed, "\n"))
}

// timeoutError explains a sync that did not converge before the deadline by
// naming the app's managed resources that are not yet Healthy — the ones the
// sync was waiting on. Health is read from the warm cache (full manifests are
// cached for managed resources), so this costs no extra API calls.
func (e *Engine) timeoutError(app string, target []*unstructured.Unstructured, isManaged func(*cache.Resource) bool) error {
	stuck := e.unhealthyManaged(target, isManaged)
	if len(stuck) == 0 {
		return fmt.Errorf("sync of %q timed out before converging", app)
	}
	return fmt.Errorf("sync of %q timed out; still not healthy:\n  %s", app, strings.Join(stuck, "\n  "))
}

// unhealthyManaged returns one line per managed live resource whose health is
// worse than Healthy/Suspended, sorted for stable output. Kinds without a
// health check (ConfigMap, Service, …) report no health and are treated as
// healthy — matching how the sync waves gate.
func (e *Engine) unhealthyManaged(target []*unstructured.Unstructured, isManaged func(*cache.Resource) bool) []string {
	lives, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
	if err != nil {
		return nil
	}
	return unhealthyLines(lives)
}

// unhealthyLines describes the live objects that are worse than Healthy (or
// Suspended, which is intentional), one sorted line each. Kinds without a
// health check (ConfigMap, Service, …) report no health and are omitted — the
// sync waves treat them as immediately healthy, so they are never what a sync
// waits on.
func unhealthyLines(lives map[kube.ResourceKey]*unstructured.Unstructured) []string {
	var lines []string
	for key, obj := range lives {
		h, err := health.GetResourceHealth(obj, nil)
		if err != nil || h == nil {
			continue
		}
		if h.Status == health.HealthStatusHealthy || h.Status == health.HealthStatusSuspended {
			continue
		}
		line := fmt.Sprintf("%s: %s", key.String(), h.Status)
		if h.Message != "" {
			line += " — " + strings.TrimSpace(h.Message)
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

// fillDefaultNamespace sets namespace on namespaced objects that carry none —
// what ArgoCD does with destination.namespace before diffing and syncing.
// Cluster-scoped and unknown-scope objects are left untouched (the engine
// resolves unknown scopes again at task creation, with the same default).
func fillDefaultNamespace(objs []*unstructured.Unstructured, namespace string, isNamespaced func(gk schema.GroupKind) (bool, error)) {
	for _, obj := range objs {
		if obj.GetNamespace() != "" {
			continue
		}
		if namespaced, err := isNamespaced(obj.GroupVersionKind().GroupKind()); err == nil && namespaced {
			obj.SetNamespace(namespace)
		}
	}
}

// alignedLiveObjs returns the live counterpart of every target object (nil
// where none exists), in target order — the pairing diff.DiffArray expects.
// Extra entries in lives (live-only resources, i.e. prune candidates) have no
// target to diff against and are dropped; prune handles them.
func alignedLiveObjs(target []*unstructured.Unstructured, lives map[kube.ResourceKey]*unstructured.Unstructured) []*unstructured.Unstructured {
	aligned := make([]*unstructured.Unstructured, len(target))
	for i, t := range target {
		aligned[i] = lives[kube.GetResourceKey(t)]
	}
	return aligned
}

// createNamespaceIfMissing is ksync's namespace auto-creation contract — the
// behavior of ArgoCD's CreateNamespace=true without managed namespace
// metadata: create the app's default namespace when absent, never touch an
// existing one (returning true for an existing namespace would overwrite its
// metadata).
func createNamespaceIfMissing(_, live *unstructured.Unstructured) (bool, error) {
	return live == nil, nil
}

// StampTracking returns copies of objs labeled as belonging to app. Copies,
// because callers reuse the rendered objects (e.g. for diff output).
func StampTracking(app string, objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, len(objs))
	for i, obj := range objs {
		c := obj.DeepCopy()
		labels := c.GetLabels()
		if labels == nil {
			labels = make(map[string]string, 1)
		}
		labels[TrackingLabel] = app
		c.SetLabels(labels)
		out[i] = c
	}
	return out
}

// revision identifies the synced content in results and logs. ksync syncs
// working trees, not commits, so the "revision" is a content hash of the
// target manifests (json.Marshal sorts map keys, making it deterministic).
func revision(objs []*unstructured.Unstructured) string {
	h := sha256.New()
	for _, o := range objs {
		b, _ := json.Marshal(o.Object)
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
