package render

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResultImages(t *testing.T) {
	objs := []*unstructured.Unstructured{
		// Deployment: main container + init container under spec.template.spec.
		obj(map[string]any{
			"kind": "Deployment",
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"initContainers": []any{map[string]any{"image": "busybox:1.36"}},
				"containers":     []any{map[string]any{"image": "ghcr.io/team-a/api:v1"}},
				"volumes": []any{map[string]any{"image": map[string]any{
					"reference": "registry.example.com/data:2",
				}}},
			}}},
		}),
		// CronJob: image nested under the job template.
		obj(map[string]any{
			"kind": "CronJob",
			"spec": map[string]any{"jobTemplate": map[string]any{"spec": map[string]any{
				"template": map[string]any{"spec": map[string]any{
					"containers": []any{map[string]any{"image": "alpine:3.20"}},
				}},
			}}},
		}),
		// Bare Pod: image directly under spec.
		obj(map[string]any{
			"kind": "Pod",
			"spec": map[string]any{
				"containers": []any{map[string]any{"image": "redis:7"}},
			},
		}),
		// A container with no image field is skipped, not panicked on.
		obj(map[string]any{
			"kind": "Pod",
			"spec": map[string]any{"containers": []any{map[string]any{"name": "x"}}},
		}),
	}

	got := (&Result{Objects: objs}).Images()
	// Emission order: containers, then initContainers, then volume refs per base,
	// in object order. The command canonicalizes and sorts, so only the set matters.
	want := []string{
		"ghcr.io/team-a/api:v1",
		"busybox:1.36",
		"registry.example.com/data:2",
		"alpine:3.20",
		"redis:7",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Images() = %v, want %v", got, want)
	}
}

func obj(m map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: m}
}
