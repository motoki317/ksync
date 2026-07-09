package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sync "github.com/argoproj/argo-cd/gitops-engine/pkg/sync"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube/kubetest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/util/openapi"
)

// These are CONTRACT tests: they pin three undocumented gitops-engine internals
// that Sync's convergence retry (sync.go, converge.go) silently depends on. A
// future engine bump can change these semantics without renaming any exported
// type — the code would still compile and the pure-helper tests (converge_test.go)
// would still pass — yet the retry loop would break at runtime. So each test
// drives the real exported sync.NewSyncContext(...)/Sync() path (the same one
// sync.go:314-341 calls) and asserts the operation phase and whether the mock's
// apply was invoked, not the shape of any Go type.
//
// The invariants, at engine v0.0.0-20260528113041-1801122b4391:
//
//  1. A carried completed-unsuccessful task result, re-seeded via WithInitialState,
//     short-circuits the operation straight to OperationFailed WITHOUT re-running
//     the task (pkg/sync/sync_context.go:619-625: "if there are any completed but
//     unsuccessful tasks, sync is a failure").
//  2. If that result is STRIPPED (absent from WithInitialState's results), the task
//     is pending (operationState == "", sync_context.go:629 filters on t.pending(),
//     pkg/sync/sync_task.go:94) and is APPLIED AGAIN.
//  3. A kept SUCCEEDED result (HookPhase Succeeded) is completed & successful, so it
//     is neither caught by the :621 failure filter nor kept by the :629 pending
//     filter — it is dropped from the task list and NOT re-run.
//
// The lever tying task state to the seeded result is sync_context.go:1129-1136,
// which sets task.operationState = result.HookPhase; task.pending()/completed()/
// successful() (sync_task.go:94,102,106) read that operationState. So it is the
// carried result's HookPhase — exactly what ksync's stripFailedResults keys on —
// that decides re-run vs short-circuit.

const (
	contractName      = "shop"
	contractNamespace = "team-a"
)

// recordingKubectl injects a MockResourceOps the test holds a reference to.
// gitops-engine's MockKubectlCmd.ManageResources hands back a fresh, throwaway
// MockResourceOps (pkg/utils/kube/kubetest/mock.go), so through the exported
// NewSyncContext there is otherwise no way to observe whether the sync applied a
// task — the very thing these invariants turn on. Embedding MockKubectlCmd
// promotes every other Kubectl method; only ManageResources is overridden.
type recordingKubectl struct {
	*kubetest.MockKubectlCmd
	ops *kubetest.MockResourceOps
}

func (k *recordingKubectl) ManageResources(*rest.Config, openapi.Resources) (kube.ResourceOperations, func(), error) {
	return k.ops, func() {}, nil
}

// discoveryServer serves the one real API call the sync path makes for a plain
// Pod: getSyncTasks (sync_context.go:1038) discovers the target's server resource
// to validate permissions, via the real discovery client NewSyncContext builds
// from restConfig (which the exported API gives no seam to replace). Without a
// reachable endpoint getSyncTasks fails with "tasks are not valid" and the run
// never reaches the completed/pending short-circuit these tests pin. Everything
// else (apply, prune) is served by the mock kubectl/resourceOps.
func discoveryServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&metav1.APIResourceList{
				TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
				GroupVersion: "v1",
				APIResources: []metav1.APIResource{{
					Name:       "pods",
					Namespaced: true,
					Kind:       "Pod",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// contractPod is the single target resource — a plain Pod, no hooks, no waves —
// so the operation completes in one Sync() call and the only variable is the
// carried result state. Fictional placeholders only (app "shop" / namespace
// "team-a"), per the repo's no-leakage rule.
func contractPod() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": contractName, "namespace": contractNamespace},
		"spec": map[string]any{
			"containers": []any{map[string]any{"name": "app", "image": "example.com/shop:dev"}},
		},
	}}
}

// contractKey is the target Pod's resource key — the key both the seeded result
// and the generated sync task resolve to, so a seeded result actually enriches
// the task (sync_context.go:1130 keys syncRes by resourceResultKey).
func contractKey() kube.ResourceKey {
	return kube.NewResourceKey("", "Pod", contractNamespace, contractName)
}

// completedUnsuccessful is the result a failed apply attempt leaves behind: an
// apply failure carries Status SyncFailed and HookPhase Error (Error is completed
// but not successful). This is exactly what ksync's stripFailedResults must drop.
func completedUnsuccessful() common.ResourceSyncResult {
	return common.ResourceSyncResult{
		ResourceKey: contractKey(),
		SyncPhase:   common.SyncPhaseSync,
		Status:      common.ResultCodeSyncFailed,
		HookPhase:   common.OperationError,
		Message:     "the server rejected our request (simulated apply failure)",
		Order:       1,
	}
}

// succeededResult is a completed-successful apply result (Status Synced, HookPhase
// Succeeded) — the kind ksync deliberately KEEPS so it is not re-run.
func succeededResult() common.ResourceSyncResult {
	return common.ResourceSyncResult{
		ResourceKey: contractKey(),
		SyncPhase:   common.SyncPhaseSync,
		Status:      common.ResultCodeSynced,
		HookPhase:   common.OperationSucceeded,
		Order:       1,
	}
}

// runContractSync drives the exported sync path exactly as engine.Sync does
// (NewSyncContext with WithServerSideApply + WithInitialState, then Sync()), with
// the given carried results re-seeded, and reports the final operation phase and
// whether the mock's apply was invoked for the Pod.
//
// The mock's apply SUCCEEDS (no Err), so any task the engine treats as pending is
// driven to Succeeded — making "did it re-run?" observable as OperationSucceeded +
// apply-invoked, versus the short-circuit's OperationFailed + apply-not-invoked.
//
// Only the two options the invariants depend on are set: WithInitialState (the
// carried state) and WithServerSideApply (the apply path ksync uses). ksync's
// other options (prune, health override, the modification checker) gate WHICH
// resources apply, not the completed/pending short-circuit pinned here, and the
// modification checker in particular would need a real diff — orthogonal to the
// contract, so it is omitted to keep the harness cluster-free.
func runContractSync(t *testing.T, seeded []common.ResourceSyncResult) (common.OperationPhase, string, bool) {
	t.Helper()
	ops := &kubetest.MockResourceOps{
		Commands: map[string]kubetest.KubectlOutput{
			// No Err ⇒ a re-applied task succeeds.
			contractName: {Output: "pod/" + contractName + " serverside-applied"},
		},
	}
	kubectl := &recordingKubectl{MockKubectlCmd: &kubetest.MockKubectlCmd{}, ops: ops}
	// Host points at the discovery stub; the dynamic/extensions clients
	// NewSyncContext also builds from this config are never called for a plain
	// Pod (SSA with no live object skips the client-side-apply migration, and the
	// apply itself goes through the mock resourceOps).
	cfg := &rest.Config{Host: discoveryServer(t).URL}
	recRes := sync.ReconciliationResult{
		Target: []*unstructured.Unstructured{contractPod()},
		Live:   []*unstructured.Unstructured{nil}, // nothing live yet: a fresh apply
	}
	syncCtx, cleanup, err := sync.NewSyncContext(
		"contract-rev", recRes, cfg, cfg, kubectl, contractNamespace, nil,
		sync.WithServerSideApply(true),
		sync.WithInitialState(common.OperationRunning, "", seeded, metav1.Now()),
	)
	if err != nil {
		t.Fatalf("NewSyncContext: %v", err)
	}
	defer cleanup()

	syncCtx.Sync()

	phase, message, _ := syncCtx.GetState()
	applied := ops.GetLastResourceCommand(contractKey()) == "apply"
	return phase, message, applied
}

// Invariant 1: a carried completed-unsuccessful result short-circuits the whole
// operation to Failed without re-running the task (sync_context.go:619-625). This
// is why ksync MUST strip such results before re-seeding — otherwise every retry
// would return Failed for a resource it never re-attempted.
func TestContract_CompletedUnsuccessfulResultShortCircuits(t *testing.T) {
	phase, message, applied := runContractSync(t, []common.ResourceSyncResult{completedUnsuccessful()})

	if phase != common.OperationFailed {
		t.Errorf("phase = %q, want %q: a carried completed-unsuccessful result must short-circuit to Failed", phase, common.OperationFailed)
	}
	if applied {
		t.Error("apply was invoked: the short-circuit must NOT re-run the failed task (sync_context.go:619-625)")
	}
	// Guard against a false pass: a broken discovery stub would also yield Failed,
	// but via getSyncTasks' "tasks are not valid" path, NOT the completed-unsuccessful
	// short-circuit this test pins. The message must be the :623 one.
	if !strings.Contains(message, "completed unsuccessfully") {
		t.Errorf("Failed for the wrong reason: message = %q, want the sync_context.go:623 short-circuit (%q)", message, "completed unsuccessfully")
	}
}

// Invariant 2: with that same result STRIPPED by ksync's own stripFailedResults,
// the task is pending (operationState == "") and is applied again, reaching
// Succeeded on a mock that now succeeds (sync_context.go:629, sync_task.go:94).
// Using stripFailedResults here ties the contract to ksync's predicate: if the
// predicate ever stopped dropping this result, this test would fail too.
func TestContract_StrippedResultReRuns(t *testing.T) {
	seeded := stripFailedResults([]common.ResourceSyncResult{completedUnsuccessful()})
	if len(seeded) != 0 {
		t.Fatalf("precondition: stripFailedResults must drop the completed-unsuccessful result, got %d kept", len(seeded))
	}

	phase, _, applied := runContractSync(t, seeded)

	if !applied {
		t.Error("apply was NOT invoked: a stripped (pending) task must be re-applied (sync_context.go:629)")
	}
	if phase != common.OperationSucceeded {
		t.Errorf("phase = %q, want %q: the re-applied task should reach Succeeded on a succeeding mock", phase, common.OperationSucceeded)
	}
}

// Invariant 3: a kept SUCCEEDED result is completed & successful, so it is dropped
// from the task list (neither a failure at :621 nor pending at :629) and NOT
// re-run. This is why ksync's stripFailedResults keeps succeeded results — they
// must survive the strip yet never re-apply.
func TestContract_KeptSucceededResultNotReRun(t *testing.T) {
	phase, _, applied := runContractSync(t, []common.ResourceSyncResult{succeededResult()})

	if applied {
		t.Error("apply was invoked: a completed-successful task must NOT be re-run")
	}
	if phase != common.OperationSucceeded {
		t.Errorf("phase = %q, want %q: an all-succeeded operation with no remaining tasks is Succeeded", phase, common.OperationSucceeded)
	}
}

// Invariant 4 (the empty-HookPhase pending rule): a carried result whose HookPhase
// is empty ("") is treated as PENDING and re-applied, regardless of its Status —
// operationState governs, and operationState is seeded from HookPhase
// (sync_context.go:1133), which "" makes pending (sync_task.go:94), applied again
// at sync_context.go:629. This is the raw gitops-engine rule beneath ksync's
// strip: dropping a result (invariant 2) leaves the task with no seeded
// operationState (== ""), i.e. pending; this test pins the same pending outcome
// from the other direction — a PRESENT result whose HookPhase is "".
//
// The result shape here (Status SyncFailed, HookPhase "") is exactly what
// gitops-engine records for a dry-run / permission-validator failure
// (sync_context.go:1066). ksync's stripFailedResults deliberately KEEPS it (its
// predicate is completed-unsuccessful HookPhase only, not Status — see
// converge.go), because a non-completed result re-runs on its own and need not be
// dropped. So this pins that a kept dry-run failure is not wedged into a
// permanent short-circuit: carried forward with an empty HookPhase, the task is
// pending and re-runs. We route through stripFailedResults to tie the test to
// ksync's predicate, exactly as invariant 2 does.
func TestContract_EmptyHookPhaseResultReRuns(t *testing.T) {
	seeded := common.ResourceSyncResult{
		ResourceKey: contractKey(),
		SyncPhase:   common.SyncPhaseSync,
		Status:      common.ResultCodeSyncFailed,
		HookPhase:   "", // not completed ⇒ task.pending() is true (sync_task.go:94)
		Order:       1,
	}
	kept := stripFailedResults([]common.ResourceSyncResult{seeded})
	if len(kept) != 1 {
		t.Fatalf("precondition: stripFailedResults must KEEP an empty-HookPhase result (it re-runs on its own), got %d kept", len(kept))
	}

	phase, _, applied := runContractSync(t, kept)

	if !applied {
		t.Error("apply was NOT invoked: a result with empty HookPhase is pending and must re-run (sync_context.go:1133, sync_task.go:94)")
	}
	if phase != common.OperationSucceeded {
		t.Errorf("phase = %q, want %q: the re-applied task should reach Succeeded on a succeeding mock", phase, common.OperationSucceeded)
	}
}
