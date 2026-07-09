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
	// Force re-runs hooks even when the rendered manifests are unchanged. A
	// no-diff sync normally skips hooks (matching gitops-engine), so a PostSync
	// Job that already ran is not re-run; Force overrides that, re-running every
	// hook the way ArgoCD's manual sync does — the escape hatch for re-applying a
	// release whose source did not change. A failed hook is re-run regardless of
	// this flag (see the skipHooks decision in Sync).
	Force bool
	// OnWait, when set, is called each poll while the sync is still waiting on
	// not-yet-Healthy resources — both during the operation (a health-gated wave or
	// a non-hook Job) and in the post-apply health gate. It drives a live "waiting
	// for health" progress line. It is never called with an empty set, so an
	// already-healthy sync returns without ever invoking it.
	OnWait func(pending []ResourceStatus)
	// OnRetry, when set, is called once per failed convergence attempt (Recovered
	// false) and once when the sync first applies cleanly after >=1 failure
	// (Recovered true). It drives the failure/retry/recovery lines the command
	// layer shows in realtime; it is never called for a sync that never fails, so
	// the clean path carries no overhead. UI-neutral, like OnWait.
	OnRetry func(ev RetryEvent)
	// ServerSide decides the apply set from a dry-run server-side apply on the
	// first reconcile (so a field the cluster defaults or prunes is not seen as
	// drift and re-applied every sync), matching what `ksync diff` previews. The
	// health-wait polls that follow stay client-side — the apply set is already
	// decided, and a dry-run per poll would cost an API round-trip per resource
	// per second. When false, the apply set uses the client-side diff throughout.
	ServerSide bool
	// AllowEmpty permits a sync whose target renders to zero objects to prune the
	// app's entire managed live set. It defaults false, so an accidental
	// empty render (a broken overlay, a commented-out resource) refuses rather
	// than silently deleting the whole app — ArgoCD's allowEmpty=false guard. Only
	// `ksync destroy`, whose empty target is the intent, sets it.
	AllowEmpty bool
}

// ResourceStatus is one resource's health as the sync gate sees it — its
// identity plus a human-readable status and message — in UI-neutral terms (no
// gitops-engine types), so the command layer can render the live wait line and
// the timeout diagnostics without importing the engine's health/kube packages.
type ResourceStatus struct {
	Group, Kind, Namespace, Name string
	// Status is the health status name (Progressing/Degraded/Missing/…);
	// "Missing" means the cache has not observed the resource yet.
	Status  string
	Message string
}

func (r ResourceStatus) key() kube.ResourceKey {
	return kube.NewResourceKey(r.Group, r.Kind, r.Namespace, r.Name)
}

// ShortName is the compact "Kind/name" label for the live "waiting for health"
// line, where the full group/namespace is noise.
func (r ResourceStatus) ShortName() string { return r.Kind + "/" + r.Name }

// Line is the full "group/Kind/ns/name: Status — message" detail line used in
// the timeout error and the diagnostics header. With no Status (an intermediate
// controller surfaced only for its events, with no health verdict of its own) it
// is just the identity.
func (r ResourceStatus) Line() string {
	k := r.key()
	if r.Status == "" {
		return k.String()
	}
	line := k.String() + ": " + r.Status
	if r.Message != "" {
		line += " — " + r.Message
	}
	return line
}

// TimeoutError reports a sync that did not become healthy before its deadline,
// naming the resources still not Healthy. The command layer detects it with
// errors.As to print a diagnostic dump (events + related pods' logs); see
// Engine.Diagnose.
type TimeoutError struct {
	App     string
	Pending []ResourceStatus
	// Retries is how many times the sync re-attempted before the deadline, and
	// LastFailure is the most recent apply failure (empty when the sync never
	// failed, only never became healthy). Together they let a timeout after
	// repeated apply failures name the cause, not just the symptom.
	Retries     int
	LastFailure string
}

func (e *TimeoutError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sync of %q timed out", e.App)
	if e.Retries > 0 {
		fmt.Fprintf(&b, " after %d %s", e.Retries, retryNoun(e.Retries))
	}
	if e.LastFailure != "" {
		fmt.Fprintf(&b, "; last failure: %s", e.LastFailure)
	}
	switch {
	case len(e.Pending) > 0:
		fmt.Fprintf(&b, "; still not healthy:\n  %s", strings.Join(statusLines(e.Pending), "\n  "))
	case e.Retries == 0 && e.LastFailure == "":
		b.WriteString(" before converging")
	}
	return b.String()
}

// EmptyRenderError reports a prune refused because the app rendered to zero
// objects while it still has managed live resources — applying that with prune
// would delete the whole app. The command layer can errors.As it; in watch mode
// the next good edit re-renders and syncs normally.
type EmptyRenderError struct {
	App  string
	Live int
}

func (e *EmptyRenderError) Error() string {
	return fmt.Sprintf("%s rendered 0 resources but manages %d live resource(s); refusing to prune them all. Fix the kustomization, or run `ksync destroy %s` to remove the app intentionally.", e.App, e.Live, e.App)
}

// refusesEmptyPrune is the empty-render guard decision, split out so the rule —
// empty target, prune on, not opted into emptiness, live resources still to lose
// — is unit-testable without a cluster cache.
func refusesEmptyPrune(targetLen, liveLen int, opts SyncOptions) bool {
	return targetLen == 0 && opts.Prune && !opts.AllowEmpty && liveLen > 0
}

// Sync makes the cluster state of one app match the given rendered resources,
// with ArgoCD semantics: server-side apply, hook phases and waves from the
// resource annotations, health-gated PostSync, and tracking-label-scoped
// prune.
func (e *Engine) Sync(ctx context.Context, app string, resources []*unstructured.Unstructured, opts SyncOptions) ([]common.ResourceSyncResult, error) {
	target := StampTracking(app, resources)
	isManaged := appManaged(app)

	// Refuse to prune the whole app to nothing. A target that renders to zero
	// objects is almost always a mistake (a commented-out resource, a broken
	// overlay, a patch that matched nothing); applying it with prune would delete
	// every resource ksync manages for the app — silently, and in watch mode on
	// every save. ArgoCD guards this the same way (allowEmpty defaults false).
	// Intentional deletion is `ksync destroy`, which sets AllowEmpty. A brand-new
	// app with nothing live yet is exempt — there is nothing to lose.
	if len(target) == 0 && opts.Prune && !opts.AllowEmpty {
		live, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
		if err != nil {
			return nil, fmt.Errorf("reading live state of %q: %w", app, err)
		}
		if refusesEmptyPrune(len(target), len(live), opts) {
			return nil, &EmptyRenderError{App: app, Live: len(live)}
		}
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
	//
	// Two cases override the "no diff → skip" rule and re-run hooks even when the
	// manifests are unchanged:
	//   - Force: the operator asked for an unconditional re-run (ArgoCD's manual
	//     sync semantics) — re-apply a release whose source did not change.
	//   - a hook is currently Degraded: a PostSync Job that failed (a transient
	//     502, a backoff-limit hit) would otherwise linger failed forever, since
	//     its source no longer diffs. Re-running it (BeforeHookCreation deletes
	//     the stale Job and recreates it) retries the failure, which is what makes
	//     a `needs` edge trustworthy — a dependency's hook must actually succeed,
	//     not just have run once. A *succeeded* hook still diffs quiet and is not
	//     re-run, so the idempotent no-op fast path is unchanged.
	skipHooks := false
	firstReconcile := true
	// The server-side apply-skip decision happens on the first reconcile only —
	// build its dry-run differ once for the whole operation (a no-op when
	// client-side). Later iterations re-diff client-side: they are health-wait
	// polls whose recRes (and its prune candidates) shifts as resources converge,
	// so the per-iteration realignment must be the cheap diff, not a dry-run per
	// resource per poll. The first reconcile is where a no-build, no-hook app
	// (the common case) decides and completes, so that is where the server-side
	// skip matters; a multi-iteration app may re-apply a server-pruned field in a
	// later wave, an idempotent server-side-apply no-op.
	var ssd *differ
	if opts.ServerSide {
		// stripLabel=false: this drives the apply set, so an unlabeled-but-matching
		// resource must read as modified to be applied and thereby adopted.
		d, cleanup, err := e.newDiffer(true, false)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		ssd = d
	}
	// Converge until the app is applied-and-healthy or --timeout expires. A
	// failed attempt (a CR whose CRD is still registering, an admission webhook
	// not yet serving, an SSA conflict, a failed hook) is retried rather than
	// returned: the failed results are stripped so the next NewSyncContext
	// re-attempts exactly those tasks, and a backoff paces the attempts. On a
	// warm cluster nothing fails, so this runs byte-identically to before — the
	// retry state is only touched in an already-failing branch.
	conv := newConvergence(retryBase, retryCap)
	recovered := false // guards the single OnRetry recovery event
	missingPolls, missingBudget := 0, missingReapplyPolls
converge:
	for {
		// Re-fill default namespaces each cycle. The pre-loop fill runs before any
		// retry, so a namespaced CR whose CRD is still registering reads as
		// cluster-scoped then (IsNamespaced can only answer for kinds the cluster
		// already serves) and keeps an empty namespace. Once a later attempt
		// registers the CRD, this fills it — without which the health gate keys the
		// resource by the wrong (empty) namespace and reports it Missing forever
		// while the live object exists under its real namespace, timing out a sync
		// that in fact converged. Idempotent: it only fills an empty namespace, so
		// on the clean path (CRD already served) it is a no-op after the first fill.
		// IsNamespaced answers from the warm cache, which learns a new CRD from its
		// CRD-Added watch event — so it can still read false on the same cycle the
		// apply first registers the kind; that cycle's target stays empty-namespaced
		// and the health gate reads Missing, self-correcting on the next cycle once
		// the cache has processed the event, so convergence lags by a poll or two,
		// never stalls.
		if opts.Namespace != "" {
			fillDefaultNamespace(target, opts.Namespace, e.clusterCache.IsNamespaced)
		}
		live, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
		if err != nil {
			return results, fmt.Errorf("reading live state of %q: %w", app, err)
		}
		recRes := sync.Reconcile(target, live, opts.Namespace, e.clusterCache)
		var diffRes *diff.DiffResultList
		if firstReconcile && ssd != nil {
			diffRes, err = ssd.diffArray(recRes.Target, recRes.Live)
		} else {
			diffRes, err = diff.DiffArray(recRes.Target, recRes.Live, diff.WithLogr(e.log))
		}
		if err != nil {
			return results, fmt.Errorf("diffing %q: %w", app, err)
		}
		if firstReconcile {
			skipHooks = !opts.Force && !diffRes.Modified && !hasDegradedHook(target, live)
			firstReconcile = false
		}

		syncCtx, cleanup, err := sync.NewSyncContext(revision, recRes, e.cfg, e.cfg, e.kubectl, opts.Namespace, e.clusterCache.GetOpenAPISchema(),
			sync.WithLogr(e.log),
			sync.WithPrune(opts.Prune),
			// Supply health for custom resources gitops-engine cannot assess
			// (an ECK Elasticsearch), so a sync wave gates on the dependency
			// actually serving — without it the CR reads healthy on create and a
			// later-wave consumer races it.
			sync.WithHealthOverride(resourceHealth),
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

		if !phase.Completed() {
			// The operation is still applying a health-gated wave or awaiting a
			// hook/non-hook Job (a migration) it created — often the longest
			// stretch of a sync, and silent off a terminal without this. Stream the
			// same wait line the final gate does; SetTail de-dupes, so an unchanged
			// pending set prints nothing. Skip an empty set: hooks are excluded from
			// pendingHealth, so a lone PostSync Job yields none and a "0 not ready"
			// line would mislead. Re-reconcile next cycle so a hook created this
			// cycle has its live health read.
			if opts.OnWait != nil {
				if p := e.pendingHealth(target, isManaged); len(p) > 0 {
					opts.OnWait(p)
				}
			}
			if err := interruptibleWait(ctx, operationRefresh, func() error {
				// A health-gated wave or a hook waiting on a stuck workload
				// (ErrImagePull, CrashLoop) holds the operation here; name what it
				// is stuck on, and carry the retry count into the timeout.
				return notConverged(ctx, e.timeoutError(app, target, isManaged, conv.retries, conv.lastReason))
			}); err != nil {
				return results, err
			}
			continue converge
		}

		if operationFailed(phase, results) {
			// gitops-engine reports task-level apply failures via the result phase,
			// not the operation error. Retry rather than give up: strip the failed
			// results so the next attempt re-runs exactly those tasks (the fresh
			// per-iteration REST mapper re-resolves a just-registered CRD's kind),
			// back off, and try again until the app converges or --timeout expires.
			reason := failureReason(message, results)
			delay := conv.fail(failedResultKeys(results), reason)
			if opts.OnRetry != nil {
				opts.OnRetry(RetryEvent{Attempt: conv.retries, NextDelay: delay, Reason: reason, Retries: conv.retries})
			}
			results = stripFailedResults(results)
			phase, message = common.OperationRunning, ""
			if err := interruptibleWait(ctx, delay, func() error {
				return notConverged(ctx, e.timeoutError(app, target, isManaged, conv.retries, conv.lastReason))
			}); err != nil {
				return results, err
			}
			continue converge
		}

		// The apply succeeded. If it took retries to get here, announce the
		// recovery once before gating on health.
		if !recovered && conv.retries > 0 {
			recovered = true
			if opts.OnRetry != nil {
				opts.OnRetry(RetryEvent{Recovered: true, Attempt: conv.retries, Retries: conv.retries})
			}
		}

		// gitops-engine marks the operation Succeeded the moment the FINAL wave is
		// applied — for an app with no sync waves or hooks it explicitly does not
		// wait for those resources to become Healthy ("a sync equates to simply an
		// asynchronous kubectl apply", sync_context.go). Gate the rest here so a
		// completed Sync means applied AND healthy — what makes a `needs` edge
		// meaningful and lets a still-converging app show as in progress instead of
		// a premature success.
		for {
			pending := e.pendingHealth(target, isManaged)
			if len(pending) == 0 {
				return results, nil
			}
			if opts.OnWait != nil {
				opts.OnWait(pending)
			}
			// A target still Missing after the apply reported success may have
			// silently never been created (a CR applied against a stale REST mapping
			// before its CRD registered). Re-apply it — a fresh apply re-resolves the
			// mapping — but only once it has stayed Missing across a few polls, so a
			// resource merely not yet observed by the cache on the clean path is not
			// re-applied; and back the poll budget off so a genuinely wedged resource
			// does not re-apply on a hot loop. Its result reads Succeeded, so strip
			// it by key to force the re-attempt.
			if hasMissing(pending) {
				missingPolls++
				if missingPolls >= missingBudget {
					missingPolls = 0
					if missingBudget < missingReapplyMax {
						missingBudget *= 2
					}
					results = stripResultKeys(results, missingKeys(pending))
					phase, message = common.OperationRunning, ""
					continue converge
				}
			} else {
				missingPolls, missingBudget = 0, missingReapplyPolls
			}
			if err := interruptibleWait(ctx, operationRefresh, func() error {
				return notConverged(ctx, notHealthyError(app, pending, conv.retries, conv.lastReason))
			}); err != nil {
				return results, err
			}
		}
	}
}

// timeoutError explains a sync that did not converge before the deadline by
// naming the app's managed resources that are not yet Healthy — the ones the
// sync was waiting on. Health is read from the warm cache (full manifests are
// cached for managed resources), so this costs no extra API calls.
func (e *Engine) timeoutError(app string, target []*unstructured.Unstructured, isManaged func(*cache.Resource) bool, retries int, lastFailure string) error {
	return notHealthyError(app, e.unhealthyManaged(target, isManaged), retries, lastFailure)
}

// notHealthyError frames a deadline-exceeded sync as a *TimeoutError carrying
// the resources still not healthy plus how many times it retried and the last
// apply failure, so the command layer can both print the message and, via
// errors.As, gather a diagnostic dump for them.
func notHealthyError(app string, pending []ResourceStatus, retries int, lastFailure string) error {
	return &TimeoutError{App: app, Pending: pending, Retries: retries, LastFailure: lastFailure}
}

// notConverged maps a ctx-cancelled wait to its error. Only the app's own
// deadline (DeadlineExceeded) is a real timeout — a *TimeoutError carrying the
// pending resources so the command layer gathers a diagnostic dump. Any other
// cause (the parent cancelled by Ctrl-C, or a sibling app's failure aborting the
// run) returns the cancellation itself, so the command layer reports a clean
// interrupt instead of a misleading "timed out" with diagnostics read against an
// already-dead context.
func notConverged(ctx context.Context, timeoutErr error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return timeoutErr
	}
	return ctx.Err()
}

// pendingHealth returns one entry per non-hook target resource that has not yet
// reached Healthy/Suspended — either the cache has not observed it (just
// applied) or its health is still Progressing/Degraded/Missing. Empty means the
// app has converged. Read from the warm cache, so it costs no API calls.
//
// It keys on the TARGET set (not the live set unhealthyManaged walks) so a
// resource the cache has not caught up to yet counts as pending, not as a
// premature success. Hooks are excluded: a hook Job with a delete policy is
// removed once it runs, so it is legitimately absent and must never hold the
// gate open. Kinds without a health check (ConfigMap, Service, CRD, custom
// resources, …) are ready as soon as they exist.
func (e *Engine) pendingHealth(target []*unstructured.Unstructured, isManaged func(*cache.Resource) bool) []ResourceStatus {
	lives, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
	if err != nil {
		// A transient read failure must not be read as convergence; report it as a
		// non-empty entry so the gate keeps waiting.
		return []ResourceStatus{{Name: "live state", Status: "Unknown", Message: fmt.Sprintf("reading live state: %v", err)}}
	}
	return pending(target, lives)
}

// pending is the pure core of pendingHealth: given the target set and the live
// objects keyed by resource key, return one ResourceStatus per non-hook target
// that has not reached Healthy/Suspended. Split out so the gate rule —
// target-keyed, hook-excluded, presence-required — is unit-testable without a
// cluster cache.
func pending(target []*unstructured.Unstructured, lives map[kube.ResourceKey]*unstructured.Unstructured) []ResourceStatus {
	var out []ResourceStatus
	for _, t := range target {
		if hook.IsHook(t) {
			continue
		}
		key := kube.GetResourceKey(t)
		live := lives[key]
		if live == nil {
			out = append(out, ResourceStatus{
				Group: key.Group, Kind: key.Kind, Namespace: key.Namespace, Name: key.Name,
				Status: "Missing", Message: "not yet created",
			})
			continue
		}
		if rs, ok := unhealthyStatus(key, live); ok {
			out = append(out, rs)
		}
	}
	sortStatuses(out)
	return out
}

// pendingLines formats the pending statuses as their detail lines — kept as the
// unit-test seam for the gate rule (the strings are what TestPendingLines reads).
func pendingLines(target []*unstructured.Unstructured, lives map[kube.ResourceKey]*unstructured.Unstructured) []string {
	return statusLines(pending(target, lives))
}

// unhealthyStatus returns the ResourceStatus for a live object whose health is
// worse than Healthy/Suspended, or false when it is healthy or has no health
// check (ConfigMap, Service, …) — those never hold a sync. An assessment error
// fails closed: it is reported (true) rather than read as healthy, so a resource
// ksync cannot evaluate holds the gate instead of letting it pass silently.
func unhealthyStatus(key kube.ResourceKey, live *unstructured.Unstructured) (ResourceStatus, bool) {
	h, err := health.GetResourceHealth(live, resourceHealth)
	if err != nil {
		return ResourceStatus{
			Group: key.Group, Kind: key.Kind, Namespace: key.Namespace, Name: key.Name,
			Status: string(health.HealthStatusUnknown), Message: strings.TrimSpace(err.Error()),
		}, true
	}
	if h == nil || h.Status == health.HealthStatusHealthy || h.Status == health.HealthStatusSuspended {
		return ResourceStatus{}, false
	}
	return ResourceStatus{
		Group: key.Group, Kind: key.Kind, Namespace: key.Namespace, Name: key.Name,
		Status: string(h.Status), Message: strings.TrimSpace(h.Message),
	}, true
}

// sortStatuses orders statuses by their detail line, so output is stable.
func sortStatuses(ss []ResourceStatus) {
	sort.Slice(ss, func(i, j int) bool { return ss[i].Line() < ss[j].Line() })
}

// statusLines renders each status as its detail line, in the slice's order.
func statusLines(ss []ResourceStatus) []string {
	lines := make([]string, len(ss))
	for i, s := range ss {
		lines[i] = s.Line()
	}
	return lines
}

// hasDegradedHook reports whether any hook in the target set has a live object
// that is currently Degraded — a PostSync Job that failed or hit its backoff
// limit. It is what lets Sync re-run a failed hook on an otherwise no-diff sync:
// the hook's own source stops diffing once it exists, so without this a failure
// would never retry. Keyed by the target hook's resource key against the live
// map (the same GetManagedLiveObjs result the diff uses), so it costs no extra
// API calls. A hook that is absent (deleted by its delete policy) or
// Healthy/Progressing is not degraded and does not trigger a re-run — only a
// genuinely-broken one does, keeping the idempotent no-op fast path intact.
func hasDegradedHook(target []*unstructured.Unstructured, live map[kube.ResourceKey]*unstructured.Unstructured) bool {
	for _, t := range target {
		if !hook.IsHook(t) {
			continue
		}
		obj := live[kube.GetResourceKey(t)]
		if obj == nil {
			continue
		}
		h, err := health.GetResourceHealth(obj, resourceHealth)
		if err != nil || h == nil {
			continue
		}
		if h.Status == health.HealthStatusDegraded {
			return true
		}
	}
	return false
}

// unhealthyManaged returns one entry per managed live resource whose health is
// worse than Healthy/Suspended, sorted for stable output. Kinds without a
// health check (ConfigMap, Service, …) report no health and are treated as
// healthy — matching how the sync waves gate.
func (e *Engine) unhealthyManaged(target []*unstructured.Unstructured, isManaged func(*cache.Resource) bool) []ResourceStatus {
	lives, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
	if err != nil {
		return nil
	}
	return unhealthyStatuses(lives)
}

// unhealthyStatuses describes the live objects worse than Healthy/Suspended, one
// ResourceStatus each, sorted. It is the struct form of the timeout/diagnostics
// view — Kinds without a health check are omitted, as the sync waves treat them
// as immediately healthy.
func unhealthyStatuses(lives map[kube.ResourceKey]*unstructured.Unstructured) []ResourceStatus {
	var out []ResourceStatus
	for key, obj := range lives {
		if rs, ok := unhealthyStatus(key, obj); ok {
			out = append(out, rs)
		}
	}
	sortStatuses(out)
	return out
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
	isManaged := appManaged(app)
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
		h, err := health.GetResourceHealth(obj, resourceHealth)
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
// apps — e.g. a controller that fans RBAC out across the app namespaces —
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
// because callers reuse the rendered objects (e.g. for diff output). A Namespace
// is copied through unlabeled: it is applied but must never become a prune
// candidate (see appManaged), matching how auto-created namespaces are left bare.
func StampTracking(app string, objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, len(objs))
	for i, obj := range objs {
		c := obj.DeepCopy()
		if !isNamespace(kube.GetResourceKey(c)) {
			labels := c.GetLabels()
			if labels == nil {
				labels = make(map[string]string, 1)
			}
			labels[TrackingLabel] = app
			c.SetLabels(labels)
		}
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
