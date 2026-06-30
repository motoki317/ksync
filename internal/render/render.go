// Package render builds kustomization directories into Kubernetes objects
// with the same semantics as the kustomize invocation production ArgoCD
// setups use: `kustomize build --enable-helm --load-restrictor
// LoadRestrictionsNone`. Renderer rationale and the version-pinning contract:
// docs/ADR/20260612-in-process-kustomize-renderer.md.
package render

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/filters/imagetag"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

// Image is one image override, with kustomize `images:` field semantics.
type Image = types.Image

// imageFieldSpecs mirrors the default field specs of kustomize's builtin
// images transformer (api/internal/konfig/builtinpluginconsts). Together with
// the recursive containers/initContainers filter below they make SetImages
// behave exactly like an `images:` entry in the kustomization. Replicated
// because kustomize keeps the canonical list in an internal package.
//
// A kustomization may extend these through its `configurations:` files (see
// configImageFieldSpecs); SetImages appends those so a built image is rewritten
// at non-standard paths too — e.g. a CRD's spec/templates/container/image —
// matching kustomize's own images transformer.
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
	// ClientRenderCommand is the binary used to inflate helmCharts for an app
	// rendered with Render(dir, clientRender=true) — a no-lookup `helm template`
	// that keeps cluster capabilities but skips the server-side dry-run. Empty
	// falls back to HelmCommand, so an offline renderer (both empty → plain helm)
	// renders every app the same way.
	ClientRenderCommand string
}

// helmCommand picks the helm binary for one render: the client-render command
// when that app opted in and the command is configured, else the default.
func (o Options) helmCommand(clientRender bool) string {
	if clientRender && o.ClientRenderCommand != "" {
		return o.ClientRenderCommand
	}
	return o.HelmCommand
}

// Result is the rendered output of one kustomization directory.
type Result struct {
	// Objects are the rendered resources in build order, decoded for the sync
	// engine.
	Objects []*unstructured.Unstructured
	resMap  resmap.ResMap
	// extraImageFieldSpecs are the image field specs the kustomization declares
	// via `configurations:` (beyond the builtin imageFieldSpecs). Captured at
	// Render time, where the kustomization directory is known, and applied by
	// SetImages so dynamic dev tags reach the extra paths.
	extraImageFieldSpecs types.FsSlice
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
	// Builtin specs plus whatever the kustomization's `configurations:` add, so
	// CRD-embedded image paths (e.g. a custom resource with a nested container
	// image) are rewritten too.
	fieldSpecs := imageFieldSpecs
	if len(res.extraImageFieldSpecs) > 0 {
		fieldSpecs = append(append(types.FsSlice{}, imageFieldSpecs...), res.extraImageFieldSpecs...)
	}
	for _, img := range images {
		// The two filters of kustomize's builtin images transformer: the
		// recursive containers/initContainers walk, then the field specs (which
		// also cover pod-level OCI volume images and any configurations: paths).
		if err := res.resMap.ApplyFilter(imagetag.LegacyFilter{ImageTag: img}); err != nil {
			return fmt.Errorf("setting image %s: %w", img.Name, err)
		}
		if err := res.resMap.ApplyFilter(imagetag.Filter{ImageTag: img, FsSlice: fieldSpecs}); err != nil {
			return fmt.Errorf("setting image %s: %w", img.Name, err)
		}
	}
	objs, err := objectsFromResMap(res.resMap)
	if err != nil {
		return err
	}
	forceLocalImagePullPolicy(objs, images)
	res.Objects = objs
	return nil
}

// forceLocalImagePullPolicy rewrites `imagePullPolicy: Always` to IfNotPresent
// on every container running an image ksync just built. The injected tag is a
// content-addressed, local-only ref (`<image>:ksync-<id>`) that exists in no
// registry, so Always makes the kubelet try to pull it and fail with
// ErrImagePull/NotFound — even though the image is present locally (and, for
// separate-store clusters, imported). Only an explicit Always is touched: an
// omitted policy already defaults to IfNotPresent for the non-:latest dev tag,
// and Never/IfNotPresent already use the local image.
func forceLocalImagePullPolicy(objs []*unstructured.Unstructured, images []Image) {
	built := make(map[string]bool, len(images))
	for _, img := range images {
		name := img.Name
		if img.NewName != "" {
			name = img.NewName
		}
		built[name] = true
	}
	// The pod-spec locations of the standard workload kinds (and bare Pods);
	// jobTemplate covers CronJob.
	bases := [][]string{
		{"spec"},
		{"spec", "template", "spec"},
		{"spec", "jobTemplate", "spec", "template", "spec"},
	}
	for _, obj := range objs {
		for _, base := range bases {
			for _, field := range []string{"containers", "initContainers"} {
				path := append(append([]string{}, base...), field)
				pinContainers(obj, path, built)
			}
		}
	}
}

func pinContainers(obj *unstructured.Unstructured, path []string, built map[string]bool) {
	list, found, err := unstructured.NestedSlice(obj.Object, path...)
	if err != nil || !found {
		return
	}
	changed := false
	for _, item := range list {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		image, _ := c["image"].(string)
		if !built[imageRepo(image)] {
			continue
		}
		if c["imagePullPolicy"] == "Always" {
			c["imagePullPolicy"] = "IfNotPresent"
			changed = true
		}
	}
	if changed {
		_ = unstructured.SetNestedSlice(obj.Object, list, path...)
	}
}

// imageRepo strips the tag and digest from an image reference, leaving the
// repository (registry/name) that kustomize's image override matches on.
func imageRepo(ref string) string {
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}
	// A ':' starts the tag only when no '/' follows it (otherwise it is a
	// registry port, e.g. localhost:5000/img).
	if c := strings.LastIndexByte(ref, ':'); c >= 0 && !strings.ContainsRune(ref[c:], '/') {
		ref = ref[:c]
	}
	return ref
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

// Render builds the kustomization at dir. clientRender selects the no-lookup
// helm command (see Options.ClientRenderCommand) for this app's chart inflation.
func (r *Renderer) Render(dir string, clientRender bool) (*Result, error) {
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
	kOpts.PluginConfig.HelmConfig.Command = r.opts.helmCommand(clientRender)

	// A fresh kustomizer per call: krusty makes no concurrency promises, and
	// per-call state is what lets one Renderer serve parallel app renders.
	resMap, err := krusty.MakeKustomizer(kOpts).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", dir, cleanRenderError(err))
	}

	objs, err := objectsFromResMap(resMap)
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", dir, err)
	}
	extra, err := configImageFieldSpecs(dir)
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", dir, err)
	}
	return &Result{Objects: objs, resMap: resMap, extraImageFieldSpecs: extra}, nil
}

// configImageFieldSpecs returns the image field specs the kustomization at dir
// declares through its `configurations:` files — the mechanism kustomize's
// images transformer uses to reach image fields the builtin specs miss, such as
// a CRD's spec/templates/container/image. SetImages appends
// them so locally built dev tags are injected there too, keeping parity with
// `kustomize build` on the same kustomization.
//
// It reads only the top-level kustomization's configurations, matching the
// dev/prod shape where each per-app kustomization references a shared config
// file; specs contributed by a base or component are not collected.
func configImageFieldSpecs(dir string) (types.FsSlice, error) {
	kfile, err := findKustomization(dir)
	if err != nil {
		return nil, err
	}
	if kfile == "" {
		// Render already built dir, so a kustomization exists; missing here only
		// under an unrecognized name. Stay silent — the builtin specs still apply.
		return nil, nil
	}
	data, err := os.ReadFile(kfile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", kfile, err)
	}
	var kust struct {
		Configurations []string `json:"configurations"`
	}
	if err := yaml.Unmarshal(data, &kust); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", kfile, err)
	}
	var specs types.FsSlice
	for _, rel := range kust.Configurations {
		p := filepath.Join(dir, rel)
		cfg, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading transformer config %s: %w", p, err)
		}
		var tc struct {
			Images types.FsSlice `json:"images"`
		}
		if err := yaml.Unmarshal(cfg, &tc); err != nil {
			return nil, fmt.Errorf("parsing transformer config %s: %w", p, err)
		}
		specs = append(specs, tc.Images...)
	}
	return specs, nil
}

// findKustomization returns the path of the kustomization file in dir, or ""
// if none of the recognized names is present.
func findKustomization(dir string) (string, error) {
	for _, name := range konfig.RecognizedKustomizationFileNames() {
		p := filepath.Join(dir, name)
		switch _, err := os.Stat(p); {
		case err == nil:
			return p, nil
		case os.IsNotExist(err):
			continue
		default:
			return "", fmt.Errorf("locating kustomization in %s: %w", dir, err)
		}
	}
	return "", nil
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
