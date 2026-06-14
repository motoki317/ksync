package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/cache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/diff"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/health"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/hook"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// operationRefresh is how often the sync loop re-reconciles live state while an
// operation is still running (matching gitops-engine's own cadence). It only
// adds latency to apps that genuinely wait (hooks, health-gated waves); a
// hookless app completes on the first iteration with no poll.
const operationRefresh = time.Second

// SyncOptions tune one Sync call.
type SyncOptions struct {
	// Prune deletes tracked resources of the app that are absent from the
	// target set.
	Prune bool
	// Namespace is the app's default namespace (ArgoCD destination.namespace
	// parity): gitops-engine stamps it on target objects that set none, and
	// when non-empty the namespace itself is created on sync if missing.
	Namespace string
	// OnWait, when set, is called once per poll while the post-apply health gate
	// is still waiting, with the resources not yet Healthy. It drives a live
	// "waiting for health" progress line; it is never called once the app has
	// converged (an already-healthy sync returns without ever invoking it).
	OnWait func(pending []string)
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

	// Create any namespace this app's resources target but does not own — a
	// multi-namespace app (e.g. workflow RBAC fanned out across app namespaces)
	// would otherwise fail to apply on a fresh cluster, since gitops-engine's
	// namespace modifier only creates the app's own destination namespace. These
	// are created bare and untracked, so prune never removes them and the app
	// that does own one later adopts it unchanged.
	if err := e.ensureReferencedNamespaces(ctx, target, opts.Namespace); err != nil {
		return nil, fmt.Errorf("preparing namespaces for %q: %w", app, err)
	}

	// Drive the sync operation ourselves rather than via engine.GitOpsEngine.Sync.
	// That convenience wrapper reconciles the live state ONCE and reuses the
	// snapshot for the whole operation; a hook resource CREATED during the sync
	// (a Helm pre-install/pre-upgrade Job → ArgoCD PreSync/Sync) is therefore
	// never in the snapshot, so its live health is never read and the operation
	// waits on it forever — every first install of an app with a hook hangs
	// until the caller's timeout. ArgoCD's controller avoids this by
	// re-reconciling each cycle; we do the same. A fresh GetManagedLiveObjs +
	// Reconcile every iteration lets the created hook's health be read;
	// WithInitialState threads the accumulated results back in so completed work
	// (and the hook itself) is not re-run within the operation.
	revision := revision(target)
	startedAt := metav1.Now()
	var phase common.OperationPhase
	var message string
	var results []common.ResourceSyncResult
	// skipHooks is decided once, from the first reconcile, then held for the
	// whole operation. This reproduces gitops-engine's own rule (skip hooks when
	// the initial diff is empty) exactly, so a no-change sync behaves as before —
	// while a *changed* sync keeps hooks enabled across every re-reconcile. The
	// latter is what makes the wait correct: once a hook is created its source
	// diff goes quiet, and re-deciding skipHooks per iteration would drop the
	// still-running hook and report success early.
	skipHooks := false
	firstReconcile := true
	for {
		live, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
		if err != nil {
			return results, fmt.Errorf("reading live state of %q: %w", app, err)
		}
		recRes := sync.Reconcile(target, live, opts.Namespace, e.clusterCache)
		diffRes, err := diff.DiffArray(recRes.Target, recRes.Live, diff.WithLogr(e.log))
		if err != nil {
			return results, fmt.Errorf("diffing %q: %w", app, err)
		}
		if firstReconcile {
			skipHooks = !diffRes.Modified
			firstReconcile = false
		}

		syncCtx, cleanup, err := sync.NewSyncContext(revision, recRes, e.cfg, e.cfg, e.kubectl, opts.Namespace, e.clusterCache.GetOpenAPISchema(),
			sync.WithLogr(e.log),
			sync.WithPrune(opts.Prune),
			// Production parity: the reference ArgoCD setup applies everything
			// server-side.
			sync.WithServerSideApply(true),
			sync.WithServerSideApplyManager(fieldManager),
			// Registering a namespace modifier turns on gitops-engine's namespace
			// auto-creation for opts.Namespace (a no-op when it is empty).
			sync.WithNamespaceModifier(createNamespaceIfMissing),
			// Apply only resources whose desired state differs from live
			// (ArgoCD's ApplyOutOfSyncOnly): O(changed), not O(all), API calls.
			sync.WithResourceModificationChecker(true, diffRes),
			sync.WithSkipHooks(skipHooks),
			// Carry the operation's accumulated state across re-reconciles so a
			// completed hook/resource is recognized, not re-run.
			sync.WithInitialState(phase, message, results, startedAt),
		)
		if err != nil {
			return results, fmt.Errorf("preparing sync of %q: %w", app, err)
		}
		syncCtx.Sync()
		phase, message, results = syncCtx.GetState()
		cleanup()

		if phase.Completed() {
			// gitops-engine reports task-level apply failures via the result
			// phase, not the operation error, so surface them explicitly — else
			// a failed apply looks like success and the watch loop never retries.
			if phase == common.OperationError || phase == common.OperationFailed {
				return results, syncFailedError(app, phase, message, results)
			}
			if err := failedResultsError(results); err != nil {
				return results, err
			}
			break
		}

		select {
		case <-ctx.Done():
			// A health-gated wave or a hook waiting on a stuck workload
			// (ErrImagePull, CrashLoop) holds the operation here until the
			// deadline; name the resource the sync is stuck on.
			return results, e.timeoutError(app, target, isManaged)
		case <-time.After(operationRefresh):
		}
	}

	// The apply succeeded, but gitops-engine marks the operation Succeeded the
	// moment the FINAL wave is applied — for an app with no sync waves or hooks
	// it explicitly does not wait for those resources to become Healthy ("a sync
	// equates to simply an asynchronous kubectl apply", sync_context.go). Hooks
	// and health-gated waves were already awaited in the loop above; gate the
	// rest here so a completed Sync means applied AND healthy. That is what makes
	// a `needs` edge meaningful — a dependent must not start until what it needs
	// is actually serving — and what lets a still-converging app show as in
	// progress instead of a premature success.
	for {
		pending := e.pendingHealth(target, isManaged)
		if len(pending) == 0 {
			return results, nil
		}
		if opts.OnWait != nil {
			opts.OnWait(pending)
		}
		select {
		case <-ctx.Done():
			return results, notHealthyError(app, pending)
		case <-time.After(operationRefresh):
		}
	}
}

// syncFailedError explains an operation that ended in Failed/Error, preferring
// the per-resource failures (the actionable detail) over the bare phase message.
func syncFailedError(app string, phase common.OperationPhase, message string, results []common.ResourceSyncResult) error {
	if err := failedResultsError(results); err != nil {
		return err
	}
	return fmt.Errorf("sync of %q %s: %s", app, phase, message)
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
	return notHealthyError(app, e.unhealthyManaged(target, isManaged))
}

// notHealthyError frames a deadline-exceeded sync, listing what was still not
// healthy (or a bare message when nothing specific could be named).
func notHealthyError(app string, lines []string) error {
	if len(lines) == 0 {
		return fmt.Errorf("sync of %q timed out before converging", app)
	}
	return fmt.Errorf("sync of %q timed out; still not healthy:\n  %s", app, strings.Join(lines, "\n  "))
}

// pendingHealth returns one line per non-hook target resource that has not yet
// reached Healthy/Suspended — either the cache has not observed it (just
// applied) or its health is still Progressing/Degraded/Missing. Empty means the
// app has converged. Read from the warm cache, so it costs no API calls.
//
// It keys on the TARGET set (not the live set unhealthyLines walks) so a
// resource the cache has not caught up to yet counts as pending, not as a
// premature success. Hooks are excluded: a hook Job with a delete policy is
// removed once it runs, so it is legitimately absent and must never hold the
// gate open. Kinds without a health check (ConfigMap, Service, CRD, custom
// resources, …) are ready as soon as they exist.
func (e *Engine) pendingHealth(target []*unstructured.Unstructured, isManaged func(*cache.Resource) bool) []string {
	lives, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
	if err != nil {
		// A transient read failure must not be read as convergence; report it so
		// the gate keeps waiting.
		return []string{fmt.Sprintf("reading live state: %v", err)}
	}
	return pendingLines(target, lives)
}

// pendingLines is the pure core of pendingHealth: given the target set and the
// live objects keyed by resource key, return one line per non-hook target that
// has not reached Healthy/Suspended. Split out so the gate rule — target-keyed,
// hook-excluded, presence-required — is unit-testable without a cluster cache.
func pendingLines(target []*unstructured.Unstructured, lives map[kube.ResourceKey]*unstructured.Unstructured) []string {
	var pending []string
	for _, t := range target {
		if hook.IsHook(t) {
			continue
		}
		key := kube.GetResourceKey(t)
		live := lives[key]
		if live == nil {
			pending = append(pending, key.String()+": not yet created")
			continue
		}
		h, err := health.GetResourceHealth(live, nil)
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
		pending = append(pending, line)
	}
	sort.Strings(pending)
	return pending
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

// AppDegraded returns one line per managed resource of app whose health is
// Degraded — genuinely broken (CrashLoop, failed Job, Error pods), as opposed
// to Progressing (a rollout still in flight) or Missing (an apply the cache has
// not observed yet). Read from the warm cache, so it costs no API calls. It
// lets a completed sync report "applied, but something is broken" WITHOUT
// blocking on or false-flagging a transient rollout: a freshly applied healthy
// edit is Progressing, not Degraded, so it is never reported here.
func (e *Engine) AppDegraded(app, namespace string, objects []*unstructured.Unstructured) []string {
	target := StampTracking(app, objects)
	if namespace != "" {
		fillDefaultNamespace(target, namespace, e.clusterCache.IsNamespaced)
	}
	isManaged := func(r *cache.Resource) bool {
		info, ok := r.Info.(*resourceInfo)
		return ok && info.app == app
	}
	lives, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
	if err != nil {
		return nil
	}
	return degradedLines(lives)
}

// degradedLines describes the live objects whose health is Degraded, one sorted
// line each. Unlike unhealthyLines it deliberately ignores Progressing and
// Missing: those are the normal post-apply states of a healthy rollout, and
// flagging them would make every fresh edit look broken. The tradeoff is that a
// wedged StatefulSet stays Progressing (StatefulSets carry no progress
// deadline) and so is not reported here, whereas a wedged Deployment turns
// Degraded via ProgressDeadlineExceeded and is — accepting a blind spot for
// StatefulSets in exchange for never crying wolf on a healthy rollout.
func degradedLines(lives map[kube.ResourceKey]*unstructured.Unstructured) []string {
	var lines []string
	for key, obj := range lives {
		h, err := health.GetResourceHealth(obj, nil)
		if err != nil || h == nil || h.Status != health.HealthStatusDegraded {
			continue
		}
		line := fmt.Sprintf("%s: Degraded", key.String())
		if h.Message != "" {
			line += " — " + strings.TrimSpace(h.Message)
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
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

// ensureReferencedNamespaces creates every distinct namespace the target
// resources live in, except the app's own (own, handled by gitops-engine's
// namespace modifier) and the cluster scope (""). It exists for multi-namespace
// apps — e.g. argo-workflows fans workflow RBAC out across the app namespaces —
// which gitops-engine would otherwise fail to apply on a fresh cluster, since it
// only auto-creates the single destination namespace. Namespaces are created
// bare (no tracking label), so they are never pruned and the app that owns one
// adopts it unchanged on its own sync. Already-exists is the steady state and is
// not an error. No-op (zero API calls) for the common single-namespace app.
func (e *Engine) ensureReferencedNamespaces(ctx context.Context, target []*unstructured.Unstructured, own string) error {
	seen := map[string]bool{own: true, "": true}
	for _, t := range target {
		ns := t.GetNamespace()
		if seen[ns] {
			continue
		}
		seen[ns] = true
		_, err := e.kclient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating namespace %q: %w", ns, err)
		}
	}
	return nil
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
