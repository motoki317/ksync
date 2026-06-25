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
	yml, err := res.YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	if !strings.Contains(string(yml), "team-a-settings") {
		t.Errorf("YAML output does not contain the rendered resource name:\n%s", yml)
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
		t.Fatalf("len(Objects) = %d, want 2 (one ConfigMap per release)", len(res.Objects))
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
			yml, err := res.YAML()
			if err != nil {
				t.Fatalf("YAML: %v", err)
			}
			if !bytes.Equal(yml, out) {
				t.Errorf("in-process output differs from `kustomize build`:\n--- in-process ---\n%s\n--- kustomize build ---\n%s", yml, out)
			}
		})
	}
}

func TestSetImages_RewritesMatchingImagesEverywhere(t *testing.T) {
	res, err := New(Options{}).Render("testdata/images")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := res.SetImages([]Image{{Name: "example.com/team-a/api-b", NewTag: "ksync-012301230123"}}); err != nil {
		t.Fatalf("SetImages: %v", err)
	}

	want := "example.com/team-a/api-b:ksync-012301230123"
	containerImage := func(obj *unstructured.Unstructured, listField string, fields ...string) string {
		t.Helper()
		list, found, err := unstructured.NestedSlice(obj.Object, append(fields, listField)...)
		if err != nil || !found || len(list) == 0 {
			t.Fatalf("no %s at %v: found=%v err=%v", listField, fields, found, err)
		}
		img, _ := list[0].(map[string]any)["image"].(string)
		return img
	}

	deploy := findObject(t, res.Objects, "Deployment", "api-b")
	podSpec := []string{"spec", "template", "spec"}
	if got := containerImage(deploy, "containers", podSpec...); got != want {
		t.Errorf("Deployment container image = %q, want %q", got, want)
	}
	if got := containerImage(deploy, "initContainers", podSpec...); got != want {
		t.Errorf("Deployment initContainer image = %q, want %q", got, want)
	}
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if got, _ := containers[1].(map[string]any)["image"].(string); got != "example.com/shop/proxy:v1" {
		t.Errorf("unrelated image = %q, must stay untouched", got)
	}

	// A tag-less reference and a nesting the default field specs do not list
	// (CronJob pod template) must both be rewritten — kustomize `images:`
	// parity comes from the same recursive containers/initContainers filter.
	cron := findObject(t, res.Objects, "CronJob", "api-b-report")
	if got := containerImage(cron, "containers", "spec", "jobTemplate", "spec", "template", "spec"); got != want {
		t.Errorf("CronJob container image = %q, want %q", got, want)
	}

	yml, err := res.YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	if !strings.Contains(string(yml), want) {
		t.Error("YAML() does not reflect the injected tag; Objects and YAML must stay consistent")
	}
	if strings.Contains(string(yml), "example.com/team-a/api-b:main") {
		t.Error("YAML() still contains the original tag")
	}
}

// A digest override (the image-override path for a pinned, registry-resolved
// ref) rewrites the reference to name@digest, not name:tag.
func TestSetImages_Digest(t *testing.T) {
	res, err := New(Options{}).Render("testdata/images")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	const digest = "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	if err := res.SetImages([]Image{{Name: "example.com/team-a/api-b", Digest: digest}}); err != nil {
		t.Fatalf("SetImages: %v", err)
	}
	yml, err := res.YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	want := "example.com/team-a/api-b@" + digest
	if !strings.Contains(string(yml), want) {
		t.Errorf("YAML() does not contain the digest-pinned ref %q", want)
	}
}

// SetImages honors image field specs the kustomization adds through
// `configurations:`, so a built image is rewritten at a path the builtin specs
// miss — here a Pipeline CRD's spec/templates[].container.image — while a builtin
// Deployment path is still rewritten and an unrelated image is left alone. This is
// the parity that lets dev tags reach CRD-embedded images.
func TestSetImages_ConfigurationsFieldSpec(t *testing.T) {
	res, err := New(Options{}).Render("testdata/configimages")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if err := res.SetImages([]Image{{Name: "example.com/team-a/api-b", NewTag: "ksync-012301230123"}}); err != nil {
		t.Fatalf("SetImages: %v", err)
	}
	want := "example.com/team-a/api-b:ksync-012301230123"

	deploy := findObject(t, res.Objects, "Deployment", "api-b")
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if got, _ := containers[0].(map[string]any)["image"].(string); got != want {
		t.Errorf("Deployment image = %q, want %q (builtin field spec)", got, want)
	}

	wf := findObject(t, res.Objects, "Pipeline", "api-b-job")
	templates, found, err := unstructured.NestedSlice(wf.Object, "spec", "templates")
	if err != nil || !found {
		t.Fatalf("Pipeline templates: found=%v err=%v", found, err)
	}
	image := func(tmpl any) string {
		c, _ := tmpl.(map[string]any)["container"].(map[string]any)
		s, _ := c["image"].(string)
		return s
	}
	if got := image(templates[0]); got != want {
		t.Errorf("Pipeline matched image = %q, want %q (configurations: field spec)", got, want)
	}
	if got := image(templates[1]); got != "example.com/shop/proxy:v1" {
		t.Errorf("unrelated Pipeline image = %q, must stay untouched", got)
	}
}

func TestForceLocalImagePullPolicy(t *testing.T) {
	container := func(image, policy string) map[string]any {
		c := map[string]any{"name": "c", "image": image}
		if policy != "" {
			c["imagePullPolicy"] = policy
		}
		return c
	}
	deploy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"initContainers": []any{container("example.com/team-a/api-b:ksync-abc", "Always")},
			"containers": []any{
				container("example.com/team-a/api-b:ksync-abc", "Always"), // built + Always -> pinned
				container("example.com/shop/proxy:v1", "Always"),          // unrelated -> untouched
			},
		}}},
	}}
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{
				container("example.com/team-a/api-b:ksync-abc", "Never"), // explicit Never -> left as is
			},
		}}},
	}}

	forceLocalImagePullPolicy([]*unstructured.Unstructured{deploy, job},
		[]Image{{Name: "example.com/team-a/api-b", NewTag: "ksync-abc"}})

	policyAt := func(obj *unstructured.Unstructured, field string, i int) string {
		list, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
		p, _ := list[i].(map[string]any)["imagePullPolicy"].(string)
		return p
	}
	if got := policyAt(deploy, "containers", 0); got != "IfNotPresent" {
		t.Errorf("built image with Always: policy = %q, want IfNotPresent", got)
	}
	if got := policyAt(deploy, "initContainers", 0); got != "IfNotPresent" {
		t.Errorf("built initContainer with Always: policy = %q, want IfNotPresent", got)
	}
	if got := policyAt(deploy, "containers", 1); got != "Always" {
		t.Errorf("unrelated image: policy = %q, want Always untouched", got)
	}
	if got := policyAt(job, "containers", 0); got != "Never" {
		t.Errorf("explicit Never: policy = %q, want Never untouched", got)
	}
}

func TestImageRepo(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/org/img:ksync-abc":     "ghcr.io/org/img",
		"ghcr.io/org/img":               "ghcr.io/org/img",
		"localhost:5000/img":            "localhost:5000/img", // port colon, no tag
		"localhost:5000/img:v1":         "localhost:5000/img",
		"img@sha256:deadbeef":           "img",
		"ghcr.io/org/img:v1@sha256:abc": "ghcr.io/org/img",
	}
	for in, want := range cases {
		if got := imageRepo(in); got != want {
			t.Errorf("imageRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func findObject(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, obj := range objs {
		if obj.GetKind() == kind && obj.GetName() == name {
			return obj
		}
	}
	t.Fatalf("no %s %q in rendered objects", kind, name)
	return nil
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
