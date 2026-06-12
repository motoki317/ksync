// Package render builds kustomization directories into Kubernetes objects
// with the same semantics as the kustomize invocation production ArgoCD
// setups use: `kustomize build --enable-helm --load-restrictor
// LoadRestrictionsNone`. Renderer rationale and the version-pinning contract:
// docs/ADR/20260612-in-process-kustomize-renderer.md.
package render

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Options configures a Renderer.
type Options struct {
	// HelmCommand is the binary used for helmCharts inflation; empty means
	// "helm" from PATH.
	HelmCommand string
}

// Result is the rendered output of one kustomization directory.
type Result struct {
	// YAML is the multi-document build output, byte-identical to what the
	// kustomize binary would print (the parity contract with ArgoCD).
	YAML []byte
	// Objects are the same resources in build order, decoded for the sync
	// engine.
	Objects []*unstructured.Unstructured
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

	yml, err := resMap.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", dir, err)
	}
	objs := make([]*unstructured.Unstructured, 0, resMap.Size())
	for _, res := range resMap.Resources() {
		// Not res.Map(): kyaml yields YAML-typed values (plain int), which
		// break unstructured's JSON-types-only contract — DeepCopy panics on
		// them during sync. The JSON round-trip converts numbers to
		// int64/float64 as apimachinery requires.
		data, err := res.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("rendering %s: encoding %s: %w", dir, res.CurId(), err)
		}
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(data); err != nil {
			return nil, fmt.Errorf("rendering %s: decoding %s: %w", dir, res.CurId(), err)
		}
		objs = append(objs, obj)
	}
	return &Result{YAML: yml, Objects: objs}, nil
}
