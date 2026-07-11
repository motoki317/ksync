package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/health"
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

// Regression for the CRD-race health-gate bug. It verifies the fill+match
// MECHANISM the convergence loop depends on — not the per-cycle placement of the
// fill call, which lives in Sync's loop and is covered by the live e2e. A
// namespaced CR whose CRD is not yet served reads as cluster-scoped, so
// fillDefaultNamespace skips it and its target key keeps an empty namespace while
// the live object is under the app namespace — so pending() reports the (present)
// resource Missing. Once the CRD is served a re-fill sets the namespace, the key
// matches the live object, and pending() clears. The bug was that the fill ran
// only once, before the CRD registered; the loop now re-fills each cycle.
func TestFillDefaultNamespace_RefillMatchesLiveOnceCRDRegisters(t *testing.T) {
	route := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "gateway.example.com/v1",
			"kind":       "Route",
			"metadata":   map[string]any{"name": "main"}, // no namespace: relies on the app default
		}}
	}
	target := []*unstructured.Unstructured{route()}

	// The live object as the cluster (and the warm cache) holds it: created under
	// the app namespace by the apply that finally succeeded.
	liveRoute := route()
	liveRoute.SetNamespace("team-a")
	lives := map[kube.ResourceKey]*unstructured.Unstructured{kube.GetResourceKey(liveRoute): liveRoute}

	// Before the CRD is served IsNamespaced cannot classify the kind, so the fill
	// is a no-op and the target key keys by an empty namespace — the health gate
	// reports the (actually present) resource as Missing.
	crdAbsent := func(schema.GroupKind) (bool, error) { return false, errors.New("no matches for kind") }
	fillDefaultNamespace(target, "team-a", crdAbsent)
	if got := target[0].GetNamespace(); got != "" {
		t.Fatalf("namespace before CRD registers = %q, want empty (kind not yet classifiable)", got)
	}
	if p := pending(target, lives); len(p) != 1 || p[0].Status != "Missing" {
		t.Fatalf("pending before re-fill = %+v, want the live object reported Missing (the bug)", p)
	}

	// The CRD registers; the next converge cycle re-fills, the target key now
	// carries the real namespace, and the health gate matches the live object.
	crdServed := func(schema.GroupKind) (bool, error) { return true, nil }
	fillDefaultNamespace(target, "team-a", crdServed)
	if got := target[0].GetNamespace(); got != "team-a" {
		t.Fatalf("namespace after CRD registers = %q, want team-a (re-fill must apply)", got)
	}
	if p := pending(target, lives); len(p) != 0 {
		t.Fatalf("pending after re-fill = %+v, want empty (a bare CR with no health check is healthy once matched)", p)
	}
}

func TestUnhealthyLines(t *testing.T) {
	key := func(group, kind, ns, name string) kube.ResourceKey {
		return kube.ResourceKey{Group: group, Kind: kind, Namespace: ns, Name: name}
	}
	// A Deployment whose observed generation trails the desired one assesses
	// as Progressing — the deterministic stand-in for "pods not ready yet"
	// (e.g. stuck in ErrImagePull) that a health-gated sync waits on.
	stuckDeploy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "api-b", "namespace": "team-a", "generation": int64(2)},
		"spec":       map[string]any{"replicas": int64(1)},
		"status":     map[string]any{"observedGeneration": int64(1)},
	}}
	configMap := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "settings", "namespace": "team-a"},
	}}

	lines := statusLines(unhealthyStatuses(map[kube.ResourceKey]*unstructured.Unstructured{
		key("apps", "Deployment", "team-a", "api-b"): stuckDeploy,
		key("", "ConfigMap", "team-a", "settings"):   configMap,
	}))

	if len(lines) != 1 {
		t.Fatalf("lines = %v, want exactly one (the Deployment; the ConfigMap has no health check)", lines)
	}
	for _, want := range []string{"Deployment", "api-b", string(health.HealthStatusProgressing)} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q does not mention %q", lines[0], want)
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
