package render

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRender_PlainKustomization(t *testing.T) {
	res, err := New(Options{}).Render("testdata/plain")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("len(Objects) = %d, want 1", len(res.Objects))
	}
	obj := res.Objects[0]
	if obj.GetKind() != "ConfigMap" {
		t.Errorf("Kind = %q, want ConfigMap", obj.GetKind())
	}
	if obj.GetName() != "team-a-settings" {
		t.Errorf("Name = %q, want team-a-settings (namePrefix applied)", obj.GetName())
	}
	if !strings.Contains(string(res.YAML), "team-a-settings") {
		t.Errorf("YAML output does not contain the rendered resource name:\n%s", res.YAML)
	}
}

// Rendered objects feed gitops-engine, which deep-copies them; unstructured's
// DeepCopy panics on any value outside the JSON type set (a plain int where
// int64 is required). A Deployment with numeric fields is the minimal
// reproduction.
func TestRender_ObjectsCarryJSONTypesOnly(t *testing.T) {
	res, err := New(Options{}).Render("testdata/typed")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("len(Objects) = %d, want 1", len(res.Objects))
	}
	obj := res.Objects[0]

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("DeepCopy panicked: %v (objects must contain JSON types only)", r)
		}
	}()
	_ = obj.DeepCopy()

	replicas, found, err := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	if err != nil || !found {
		t.Fatalf("NestedInt64(spec.replicas): found=%v err=%v, want an int64", found, err)
	}
	if replicas != 3 {
		t.Errorf("spec.replicas = %d, want 3", replicas)
	}
}

func TestRender_HelmChartsInflatedFromSharedChartHome(t *testing.T) {
	// The production manifest shape: chartHome points outside the
	// kustomization root and one chart is inflated as several releases.
	requireBinary(t, "helm")
	res, err := New(Options{}).Render("testdata/helm-app")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Objects) != 2 {
		t.Fatalf("len(Objects) = %d, want 2 (one ConfigMap per release):\n%s", len(res.Objects), res.YAML)
	}
	names := map[string]bool{}
	for _, obj := range res.Objects {
		names[obj.GetName()] = true
		if ns := obj.GetNamespace(); ns != "team-a" {
			t.Errorf("%s: namespace = %q, want team-a (kustomization namespace applied to inflated objects)", obj.GetName(), ns)
		}
	}
	for _, want := range []string{"api-b-config", "shop-config"} {
		if !names[want] {
			t.Errorf("rendered objects %v do not include %q", names, want)
		}
	}
}

func TestRender_MatchesKustomizeBuildOutput(t *testing.T) {
	// ksync renders in-process; production ArgoCD shells out to the kustomize
	// binary. Byte-identical output is the parity contract, so any drift
	// between the pinned kustomize/api module and the binary must fail here.
	requireBinary(t, "kustomize")
	tests := []struct {
		dir       string
		needsHelm bool
	}{
		{dir: "testdata/plain"},
		{dir: "testdata/helm-app", needsHelm: true},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			if tt.needsHelm {
				requireBinary(t, "helm")
			}
			res, err := New(Options{}).Render(tt.dir)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			out, err := exec.Command("kustomize", "build",
				"--enable-helm", "--load-restrictor", "LoadRestrictionsNone", tt.dir).Output()
			if err != nil {
				t.Fatalf("kustomize build: %v", err)
			}
			if !bytes.Equal(res.YAML, out) {
				t.Errorf("in-process output differs from `kustomize build`:\n--- in-process ---\n%s\n--- kustomize build ---\n%s", res.YAML, out)
			}
		})
	}
}

func TestRender_ErrorMentionsDirectory(t *testing.T) {
	_, err := New(Options{}).Render("testdata/does-not-exist")
	if err == nil {
		t.Fatal("Render succeeded on a missing directory")
	}
	if !strings.Contains(err.Error(), "testdata/does-not-exist") {
		t.Errorf("error %q does not mention the directory", err)
	}
}

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH", name)
	}
}
