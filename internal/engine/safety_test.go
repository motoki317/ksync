package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A Namespace must be applied but never tracked: an authored Namespace that
// carried the tracking label would become a prune candidate, so a prune or
// `ksync destroy` could delete it and cascade (k8s GC) into resources of other
// apps sharing it.
func TestStampTracking_LeavesNamespaceUnlabeled(t *testing.T) {
	ns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "shop"},
	}}
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "web", "namespace": "shop"},
	}}
	out := StampTracking("shop", []*unstructured.Unstructured{ns, dep})

	if _, labeled := out[0].GetLabels()[TrackingLabel]; labeled {
		t.Error("Namespace was stamped with the tracking label; it must pass through unlabeled")
	}
	if got := out[1].GetLabels()[TrackingLabel]; got != "shop" {
		t.Errorf("Deployment tracking label = %q, want shop", got)
	}
}

// appManaged is the single predicate that scopes prune/diff/health to an app.
// Excluding Namespaces here is what neutralizes even a Namespace a prior ksync
// version had labeled — prune candidacy is decided from the live cache, not from
// what the current run stamps.
func TestAppManaged_ExcludesNamespacesAndOtherApps(t *testing.T) {
	managed := func(kind, apiVersion, app string) bool {
		return appManaged("shop")(&cache.Resource{
			Ref:  corev1.ObjectReference{Kind: kind, APIVersion: apiVersion},
			Info: &resourceInfo{app: app},
		})
	}
	cases := []struct {
		name                  string
		kind, apiVersion, app string
		want                  bool
	}{
		{"app's Deployment", "Deployment", "apps/v1", "shop", true},
		{"app's Namespace", "Namespace", "v1", "shop", false},
		{"another app's Deployment", "Deployment", "apps/v1", "api-b", false},
		{"another app's Namespace", "Namespace", "v1", "api-b", false},
	}
	for _, c := range cases {
		if got := managed(c.kind, c.apiVersion, c.app); got != c.want {
			t.Errorf("%s: appManaged = %v, want %v", c.name, got, c.want)
		}
	}
}

// The empty-render guard must fire only when an empty target would prune live
// resources that exist, and never for destroy (AllowEmpty) or a brand-new app
// with nothing live yet.
func TestRefusesEmptyPrune(t *testing.T) {
	cases := []struct {
		name      string
		targetLen int
		liveLen   int
		opts      SyncOptions
		want      bool
	}{
		{"empty render with live resources", 0, 3, SyncOptions{Prune: true}, true},
		{"empty render, nothing live (new app)", 0, 0, SyncOptions{Prune: true}, false},
		{"destroy opts in via AllowEmpty", 0, 3, SyncOptions{Prune: true, AllowEmpty: true}, false},
		{"prune disabled", 0, 3, SyncOptions{Prune: false}, false},
		{"non-empty render", 2, 3, SyncOptions{Prune: true}, false},
	}
	for _, c := range cases {
		if got := refusesEmptyPrune(c.targetLen, c.liveLen, c.opts); got != c.want {
			t.Errorf("%s: refusesEmptyPrune = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEmptyRenderError_PointsAtDestroy(t *testing.T) {
	err := error(&EmptyRenderError{App: "shop", Live: 4})
	var ere *EmptyRenderError
	if !errors.As(err, &ere) {
		t.Fatal("EmptyRenderError must be matchable with errors.As")
	}
	msg := err.Error()
	for _, want := range []string{"shop", "4", "ksync destroy shop", "refusing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}
