package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
