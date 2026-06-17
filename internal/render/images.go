package render

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// podSpecBases are the locations of a v1.PodSpec across the standard kinds: a
// bare Pod, any workload's template, and a CronJob's nested job template. These
// are the same locations kustomize's image transformer (and SetImages) touch.
var podSpecBases = [][]string{
	{"spec"},
	{"spec", "template", "spec"},
	{"spec", "jobTemplate", "spec", "template", "spec"},
}

// Images returns every container image reference the rendered objects literally
// contain — from containers/initContainers/ephemeralContainers and OCI `image`
// volumes at the standard pod-spec locations. Order follows object/field order;
// duplicates are kept (the caller canonicalizes and dedups).
//
// It reports only what the manifests name. Images a controller derives at
// runtime — an ECK Elasticsearch's data image from spec.version, an operator's
// default sidecar — are absent from the rendered YAML and so cannot appear here;
// capturing those needs a live cluster read (see `ksync images --live`).
func (res *Result) Images() []string {
	var out []string
	for _, obj := range res.Objects {
		for _, base := range podSpecBases {
			for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
				out = append(out, containerImages(obj, path(base, field))...)
			}
			out = append(out, volumeImages(obj, path(base, "volumes"))...)
		}
	}
	return out
}

// containerImages reads the `image` of each container in the list at path.
func containerImages(obj *unstructured.Unstructured, path []string) []string {
	list, found, err := unstructured.NestedSlice(obj.Object, path...)
	if err != nil || !found {
		return nil
	}
	var out []string
	for _, item := range list {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if img, ok := c["image"].(string); ok && img != "" {
			out = append(out, img)
		}
	}
	return out
}

// volumeImages reads the `image.reference` of each OCI-image volume in the list
// at path (the kubernetes 1.31+ image volume source).
func volumeImages(obj *unstructured.Unstructured, path []string) []string {
	list, found, err := unstructured.NestedSlice(obj.Object, path...)
	if err != nil || !found {
		return nil
	}
	var out []string
	for _, item := range list {
		v, ok := item.(map[string]any)
		if !ok {
			continue
		}
		image, ok := v["image"].(map[string]any)
		if !ok {
			continue
		}
		if ref, ok := image["reference"].(string); ok && ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

// path appends field to a copy of base (base is shared across iterations).
func path(base []string, field string) []string {
	return append(append([]string{}, base...), field)
}
