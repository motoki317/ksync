// Package render builds kustomization directories into Kubernetes objects
// with the same semantics as the kustomize invocation production ArgoCD
// setups use: `kustomize build --enable-helm --load-restrictor
// LoadRestrictionsNone`. Renderer rationale and the version-pinning contract:
// docs/ADR/20260612-in-process-kustomize-renderer.md.
package render

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/filters/imagetag"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Image is one image override, with kustomize `images:` field semantics.
type Image = types.Image

// imageFieldSpecs mirrors the default field specs of kustomize's builtin
// images transformer (api/internal/konfig/builtinpluginconsts). Together with
// the recursive containers/initContainers filter below they make SetImages
// behave exactly like an `images:` entry in the kustomization. Replicated
// because kustomize keeps the canonical list in an internal package.
var imageFieldSpecs = types.FsSlice{
	{Path: "spec/containers[]/image", CreateIfNotPresent: true},
	{Path: "spec/initContainers[]/image", CreateIfNotPresent: true},
	{Path: "spec/volumes[]/image/reference", CreateIfNotPresent: true},
	{Path: "spec/template/spec/containers[]/image", CreateIfNotPresent: true},
	{Path: "spec/template/spec/initContainers[]/image", CreateIfNotPresent: true},
	{Path: "spec/template/spec/volumes[]/image/reference", CreateIfNotPresent: true},
}

// Options configures a Renderer.
type Options struct {
	// HelmCommand is the binary used for helmCharts inflation; empty means
	// "helm" from PATH.
	HelmCommand string
}

// Result is the rendered output of one kustomization directory.
type Result struct {
	// Objects are the rendered resources in build order, decoded for the sync
	// engine.
	Objects []*unstructured.Unstructured
	resMap  resmap.ResMap
}

// YAML serializes the multi-document build output, byte-identical to what
// the kustomize binary would print (the parity contract with ArgoCD). It is
// a method, not a field: serializing megabytes of YAML costs real time
// (~170ms measured on a large chart) and only `ksync render` needs it — the
// sync loop works on Objects.
func (res *Result) YAML() ([]byte, error) {
	return res.resMap.AsYaml()
}

// SetImages rewrites matching image references in the rendered output —
// identical to the user writing `images: [{name, newTag}]` in the
// kustomization, but in-process so the working tree is never mutated. This is
// how locally built dev tags are injected before sync.
func (res *Result) SetImages(images []Image) error {
	for _, img := range images {
		// The two filters of kustomize's builtin images transformer: the
		// recursive containers/initContainers walk, then the fixed field
		// specs (which also cover pod-level OCI volume images).
		if err := res.resMap.ApplyFilter(imagetag.LegacyFilter{ImageTag: img}); err != nil {
			return fmt.Errorf("setting image %s: %w", img.Name, err)
		}
		if err := res.resMap.ApplyFilter(imagetag.Filter{ImageTag: img, FsSlice: imageFieldSpecs}); err != nil {
			return fmt.Errorf("setting image %s: %w", img.Name, err)
		}
	}
	objs, err := objectsFromResMap(res.resMap)
	if err != nil {
		return err
	}
	res.Objects = objs
	return nil
}

// Renderer renders kustomization directories. It is safe for concurrent use;
// each Render call builds its own kustomizer.
type Renderer struct {
	opts Options
}

func New(opts Options) *Renderer {
	if opts.HelmCommand == "" {
		opts.HelmCommand = "helm"
	}
	return &Renderer{opts: opts}
}

// Render builds the kustomization at dir.
func (r *Renderer) Render(dir string) (*Result, error) {
	kOpts := krusty.MakeDefaultOptions()
	// ReorderOptionUnspecified is what `kustomize build` passes when no
	// --reorder flag is given (legacy ordering unless the kustomization
	// declares sortOptions) — required for byte-parity with the binary.
	kOpts.Reorder = krusty.ReorderOptionUnspecified
	// The reference manifest shape keeps shared charts in a chartHome outside
	// each kustomization root, which the default root-only restriction
	// rejects; ArgoCD setups using that shape disable load restrictions.
	kOpts.LoadRestrictions = types.LoadRestrictionsNone
	kOpts.PluginConfig.HelmConfig.Enabled = true
	kOpts.PluginConfig.HelmConfig.Command = r.opts.HelmCommand

	// A fresh kustomizer per call: krusty makes no concurrency promises, and
	// per-call state is what lets one Renderer serve parallel app renders.
	resMap, err := krusty.MakeKustomizer(kOpts).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", dir, err)
	}

	objs, err := objectsFromResMap(resMap)
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", dir, err)
	}
	return &Result{Objects: objs, resMap: resMap}, nil
}

func objectsFromResMap(resMap resmap.ResMap) ([]*unstructured.Unstructured, error) {
	objs := make([]*unstructured.Unstructured, 0, resMap.Size())
	for _, res := range resMap.Resources() {
		// Not res.Map(): kyaml yields YAML-typed values (plain int), which
		// break unstructured's JSON-types-only contract — DeepCopy panics on
		// them during sync. The JSON round-trip converts numbers to
		// int64/float64 as apimachinery requires.
		data, err := res.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("encoding %s: %w", res.CurId(), err)
		}
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(data); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", res.CurId(), err)
		}
		objs = append(objs, obj)
	}
	return objs, nil
}
