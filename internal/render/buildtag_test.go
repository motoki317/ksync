package render

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func deployImage(ns, name, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": image}},
		}}},
	}}
}

// liveDevTags carries a build repo's current dev tag forward only when the live
// cluster runs it at a recognized ksync-<12hex> tag, found wherever the image
// sits — including a CRD's non-standard path — and only when every occurrence
// agrees. Anything else (a plain tag, a missing image, a disagreeing pair) is
// omitted so the caller falls open to showing the real diff.
func TestLiveDevTags(t *testing.T) {
	crdImage := func(image string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1", "kind": "Pipeline",
			"metadata": map[string]any{"name": "job"},
			"spec": map[string]any{"templates": []any{
				map[string]any{"container": map[string]any{"image": image}},
			}},
		}}
	}
	ociVolume := func(image string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "p"},
			"spec": map[string]any{"volumes": []any{
				map[string]any{"image": map[string]any{"reference": image}},
			}},
		}}
	}

	cases := []struct {
		name  string
		objs  []*unstructured.Unstructured
		repos []string
		want  map[string]string
	}{
		{
			name:  "standard container path",
			objs:  []*unstructured.Unstructured{deployImage("team-a", "api-b", "example.com/team-a/api-b:ksync-0123456789ab")},
			repos: []string{"example.com/team-a/api-b"},
			want:  map[string]string{"example.com/team-a/api-b": "ksync-0123456789ab"},
		},
		{
			name:  "CRD non-standard path",
			objs:  []*unstructured.Unstructured{crdImage("example.com/team-a/api-b:ksync-0123456789ab")},
			repos: []string{"example.com/team-a/api-b"},
			want:  map[string]string{"example.com/team-a/api-b": "ksync-0123456789ab"},
		},
		{
			name:  "OCI volume image.reference path",
			objs:  []*unstructured.Unstructured{ociVolume("example.com/team-a/api-b:ksync-0123456789ab")},
			repos: []string{"example.com/team-a/api-b"},
			want:  map[string]string{"example.com/team-a/api-b": "ksync-0123456789ab"},
		},
		{
			name:  "non-ksync tag is omitted (fail open)",
			objs:  []*unstructured.Unstructured{deployImage("team-a", "api-b", "example.com/team-a/api-b:main")},
			repos: []string{"example.com/team-a/api-b"},
			want:  map[string]string{},
		},
		{
			name: "disagreeing dev tags for one repo are omitted",
			objs: []*unstructured.Unstructured{
				deployImage("team-a", "api-b", "example.com/team-a/api-b:ksync-0123456789ab"),
				deployImage("team-a", "api-b-2", "example.com/team-a/api-b:ksync-ffffffffffff"),
			},
			repos: []string{"example.com/team-a/api-b"},
			want:  map[string]string{},
		},
		{
			name: "a ksync ref mixed with a non-ksync ref is omitted",
			objs: []*unstructured.Unstructured{
				deployImage("team-a", "api-b", "example.com/team-a/api-b:ksync-0123456789ab"),
				deployImage("team-a", "api-b-2", "example.com/team-a/api-b:latest"),
			},
			repos: []string{"example.com/team-a/api-b"},
			want:  map[string]string{},
		},
		{
			name:  "repo not present in live is omitted",
			objs:  []*unstructured.Unstructured{deployImage("team-a", "other", "example.com/shop/proxy:ksync-0123456789ab")},
			repos: []string{"example.com/team-a/api-b"},
			want:  map[string]string{},
		},
		{
			name: "only the consistent ksync repos among several",
			objs: []*unstructured.Unstructured{
				deployImage("team-a", "api-b", "example.com/team-a/api-b:ksync-0123456789ab"),
				deployImage("shop", "proxy", "example.com/shop/proxy:v1"),
			},
			repos: []string{"example.com/team-a/api-b", "example.com/shop/proxy"},
			want:  map[string]string{"example.com/team-a/api-b": "ksync-0123456789ab"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := liveDevTags(c.objs, c.repos)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("liveDevTags = %v, want %v", got, c.want)
			}
		})
	}
}

// ReferencedRepos reports the image repos the rendered objects actually deploy,
// so `ksync diff` can drop a build entry whose image no manifest references — an
// orphaned entry must not appear, or it would produce a phantom source-ref note.
func TestReferencedRepos(t *testing.T) {
	res := &Result{Objects: []*unstructured.Unstructured{
		deployImage("team-a", "api-b", "example.com/team-a/api-b:main"),
		deployImage("shop", "proxy", "example.com/shop/proxy:v1"),
	}}
	got := res.ReferencedRepos()
	for _, repo := range []string{"example.com/team-a/api-b", "example.com/shop/proxy"} {
		if !got[repo] {
			t.Errorf("repo %q should be referenced; got %v", repo, got)
		}
	}
	if got["example.com/orphan/unused"] {
		t.Errorf("a build image no manifest references must be absent: %v", got)
	}
}

// CarryForwardBuildTags injects each carried live dev tag into the rendered
// target through the same field-spec-aware path SetImages uses, so the target's
// image matches live at a CRD-embedded path too — and reports which repos it
// carried. A repo whose live image is not a ksync dev tag is left untouched.
func TestCarryForwardBuildTags_ReachesCRDPathAndReportsCarried(t *testing.T) {
	res, err := New(Options{}).Render("testdata/configimages")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	live := []*unstructured.Unstructured{
		deployImage("team-a", "api-b", "example.com/team-a/api-b:ksync-0123456789ab"),
	}
	carried, err := res.CarryForwardBuildTags(live, []string{"example.com/team-a/api-b", "example.com/shop/proxy"})
	if err != nil {
		t.Fatalf("CarryForwardBuildTags: %v", err)
	}
	// Only api-b has a live ksync tag; shop/proxy has no live image, so it is not carried.
	if want := []string{"example.com/team-a/api-b"}; !reflect.DeepEqual(carried, want) {
		t.Errorf("carried = %v, want %v", carried, want)
	}

	want := "example.com/team-a/api-b:ksync-0123456789ab"
	wf := findObject(t, res.Objects, "Pipeline", "api-b-job")
	templates, _, _ := unstructured.NestedSlice(wf.Object, "spec", "templates")
	container, _ := templates[0].(map[string]any)["container"].(map[string]any)
	if got, _ := container["image"].(string); got != want {
		t.Errorf("Pipeline CRD image = %q, want %q (carried forward via configurations: field spec)", got, want)
	}
	deploy := findObject(t, res.Objects, "Deployment", "api-b")
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if got, _ := containers[0].(map[string]any)["image"].(string); got != want {
		t.Errorf("Deployment image = %q, want %q", got, want)
	}
}
