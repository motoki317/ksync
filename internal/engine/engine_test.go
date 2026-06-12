package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestStampTracking_LabelsCopiesWithoutMutatingInput(t *testing.T) {
	in := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":   "settings",
			"labels": map[string]any{"team": "a"},
		},
	}}
	out := StampTracking("api-b", []*unstructured.Unstructured{in})
	if got := out[0].GetLabels()[TrackingLabel]; got != "api-b" {
		t.Errorf("tracking label = %q, want api-b", got)
	}
	if got := out[0].GetLabels()["team"]; got != "a" {
		t.Errorf("existing label team = %q, want a (must be preserved)", got)
	}
	if _, stamped := in.GetLabels()[TrackingLabel]; stamped {
		t.Error("input object was mutated; StampTracking must operate on copies")
	}
}

func TestStampTracking_HandlesObjectsWithoutLabels(t *testing.T) {
	in := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "settings"},
	}}
	out := StampTracking("api-b", []*unstructured.Unstructured{in})
	if got := out[0].GetLabels()[TrackingLabel]; got != "api-b" {
		t.Errorf("tracking label = %q, want api-b", got)
	}
}

func TestCreateNamespaceIfMissing_CreatesOnlyWhenAbsent(t *testing.T) {
	live := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "team-a"},
	}}
	if create, err := createNamespaceIfMissing(nil, nil); err != nil || !create {
		t.Errorf("missing namespace: (create, err) = (%v, %v), want (true, nil)", create, err)
	}
	if create, err := createNamespaceIfMissing(nil, live); err != nil || create {
		t.Errorf("existing namespace: (create, err) = (%v, %v), want (false, nil) — ksync must never modify an existing namespace", create, err)
	}
}

func TestFillDefaultNamespace(t *testing.T) {
	obj := func(kind, ns string) *unstructured.Unstructured {
		o := map[string]any{
			"apiVersion": "v1",
			"kind":       kind,
			"metadata":   map[string]any{"name": "x"},
		}
		if ns != "" {
			o["metadata"].(map[string]any)["namespace"] = ns
		}
		return &unstructured.Unstructured{Object: o}
	}
	isNamespaced := func(gk schema.GroupKind) (bool, error) {
		switch gk.Kind {
		case "ConfigMap":
			return true, nil
		case "Namespace":
			return false, nil
		default:
			return false, errors.New("unknown scope")
		}
	}

	objs := []*unstructured.Unstructured{
		obj("ConfigMap", ""),       // namespaced, empty -> filled
		obj("ConfigMap", "team-b"), // explicit namespace -> kept
		obj("Namespace", ""),       // cluster-scoped -> untouched
		obj("Unknown", ""),         // unknown scope -> untouched (engine decides later)
	}
	fillDefaultNamespace(objs, "team-a", isNamespaced)

	if got := objs[0].GetNamespace(); got != "team-a" {
		t.Errorf("namespaced object without namespace = %q, want team-a", got)
	}
	if got := objs[1].GetNamespace(); got != "team-b" {
		t.Errorf("explicit namespace = %q, want team-b (must not be overwritten)", got)
	}
	if got := objs[2].GetNamespace(); got != "" {
		t.Errorf("cluster-scoped namespace = %q, want empty", got)
	}
	if got := objs[3].GetNamespace(); got != "" {
		t.Errorf("unknown-scope namespace = %q, want empty", got)
	}
}

func TestAlignedLiveObjs(t *testing.T) {
	cm := func(ns, name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": name, "namespace": ns},
		}}
	}
	target := []*unstructured.Unstructured{cm("team-a", "one"), cm("team-a", "two")}
	liveOne := cm("team-a", "one")
	lives := map[kube.ResourceKey]*unstructured.Unstructured{
		kube.GetResourceKey(liveOne): liveOne,
		// an extra managed live object not in target (prune candidate) must
		// not disturb alignment
		kube.GetResourceKey(cm("team-a", "gone")): cm("team-a", "gone"),
	}

	aligned := alignedLiveObjs(target, lives)
	if len(aligned) != 2 {
		t.Fatalf("len = %d, want 2 (one slot per target)", len(aligned))
	}
	if aligned[0] != liveOne {
		t.Errorf("aligned[0] = %v, want the matching live object", aligned[0])
	}
	if aligned[1] != nil {
		t.Errorf("aligned[1] = %v, want nil (no live state yet)", aligned[1])
	}
}

func TestFailedResultsError(t *testing.T) {
	ok := common.ResourceSyncResult{
		ResourceKey: kube.ResourceKey{Group: "", Kind: "ConfigMap", Namespace: "team-a", Name: "settings"},
		Status:      common.ResultCodeSynced,
	}
	failed := common.ResourceSyncResult{
		ResourceKey: kube.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "team-a", Name: "api-b"},
		Status:      common.ResultCodeSyncFailed,
		Message:     "admission webhook denied",
	}

	if err := failedResultsError([]common.ResourceSyncResult{ok, ok}); err != nil {
		t.Errorf("all-synced results produced error: %v", err)
	}

	err := failedResultsError([]common.ResourceSyncResult{ok, failed})
	if err == nil {
		t.Fatal("a SyncFailed result must produce an error — returning nil makes the watch loop treat the sync as successful and skip the retry")
	}
	for _, want := range []string{"Deployment", "api-b", "admission webhook denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRevision_ChangesWithContentOnly(t *testing.T) {
	objs := func(data string) []*unstructured.Unstructured {
		return []*unstructured.Unstructured{{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "settings"},
			"data":       map[string]any{"k": data},
		}}}
	}
	r1, r2, r3 := revision(objs("a")), revision(objs("a")), revision(objs("b"))
	if r1 != r2 {
		t.Errorf("same content produced different revisions: %q vs %q", r1, r2)
	}
	if r1 == r3 {
		t.Errorf("different content produced the same revision %q", r1)
	}
}

const kubeconfigFixture = `
apiVersion: v1
kind: Config
current-context: other
clusters:
  - name: target-cluster
    cluster: {server: "https://target.example:6443"}
  - name: other-cluster
    cluster: {server: "https://other.example:6443"}
contexts:
  - name: target
    context: {cluster: target-cluster}
  - name: other
    context: {cluster: other-cluster}
`

func TestRESTConfig_UsesTheNamedContextNotCurrentContext(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t))
	cfg, err := RESTConfig("target")
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	if cfg.Host != "https://target.example:6443" {
		t.Errorf("Host = %q, want the named context's server (current-context points elsewhere)", cfg.Host)
	}
}

func TestRESTConfig_RejectsEmptyContext(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t))
	if _, err := RESTConfig(""); err == nil {
		t.Fatal("RESTConfig(\"\") succeeded; it must never fall back to current-context")
	}
}

func TestRESTConfig_RejectsUnknownContext(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t))
	_, err := RESTConfig("ghost")
	if err == nil {
		t.Fatal("RESTConfig succeeded for a context absent from the kubeconfig")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q does not name the missing context", err)
	}
}

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfigFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
