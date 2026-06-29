package engine

import (
	"fmt"
	"sort"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/diff"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/hook"
	syncresource "github.com/argoproj/argo-cd/gitops-engine/pkg/sync/resource"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// DiffType classifies how a sync would change one resource relative to live
// cluster state.
type DiffType int

const (
	// DiffUpdate: the resource exists and its rendered form differs from live.
	DiffUpdate DiffType = iota
	// DiffCreate: the resource is in the rendered target but not live.
	DiffCreate
	// DiffPrune: the resource is a tracked live resource absent from the target,
	// so a pruning sync would delete it.
	DiffPrune
)

func (t DiffType) String() string {
	switch t {
	case DiffCreate:
		return "create"
	case DiffPrune:
		return "prune"
	default:
		return "update"
	}
}

// ResourceDiff is one resource's difference between the rendered target and live
// state, in UI-neutral terms (no gitops-engine types) so the command layer can
// render it without importing the engine's diff/kube packages. Before and After
// are the rendered YAML of the live and target sides (a Secret's values masked):
// Before is empty for a creation, After is empty for a prune, and for an update
// they are gitops-engine's normalized-live and predicted-live views.
type ResourceDiff struct {
	Group, Kind, Namespace, Name string
	Type                         DiffType
	Before, After                string
}

func (d ResourceDiff) key() kube.ResourceKey {
	return kube.NewResourceKey(d.Group, d.Kind, d.Namespace, d.Name)
}

// DiffOptions tune one Diff call.
type DiffOptions struct {
	// Prune includes tracked live resources absent from the target (what a
	// pruning sync would delete). Mirror the sync's own --prune so the preview
	// matches the apply.
	Prune bool
	// Namespace is the app's default namespace (ArgoCD destination.namespace
	// parity): filled on namespaced target objects that set none, before any
	// key-based matching against live state.
	Namespace string
	// ServerSide computes each resource's diff via a dry-run server-side apply
	// (the apiserver's predicted result), so fields the cluster defaults or
	// prunes do not read as drift. Per-resource it falls back to client-side on a
	// dry-run error. When false, the in-process three-way / structured-merge diff
	// is used.
	ServerSide bool
}

// Diff previews what a sync of one app would change: it renders nothing itself
// (the caller passes the already-rendered resources) but reproduces sync's exact
// pre-diff pipeline — stamp the tracking label, fill the default namespace, read
// live managed state, and reconcile — then reports one entry per resource a sync
// would create, update, or prune. Read-only: no apply, no hook execution.
//
// The diff engine is the same client-side three-way / structured-merge path
// sync's own apply-skip checker uses (diff.Diff), so "diff reports X" matches
// "sync would apply X"; it is not a full server-side-apply diff (see ADR
// 20260625-diff-command).
func (e *Engine) Diff(app string, resources []*unstructured.Unstructured, opts DiffOptions) ([]ResourceDiff, error) {
	target := StampTracking(app, resources)
	if opts.Namespace != "" {
		fillDefaultNamespace(target, opts.Namespace, e.clusterCache.IsNamespaced)
	}
	isManaged := appManaged(app)
	live, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
	if err != nil {
		return nil, fmt.Errorf("reading live state of %q: %w", app, err)
	}
	// The same reconcile sync runs: aligns each target with its live counterpart,
	// splits hooks out, applies the unknown-scope namespace fallback and live
	// dedup, and appends live-only managed resources as Target=nil pairs (the
	// prune candidates).
	rec := sync.Reconcile(target, live, opts.Namespace, e.clusterCache)
	// stripLabel: this is the display path, so hide a label-only adoption.
	d, cleanup, err := e.newDiffer(opts.ServerSide, true)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return diffResources(rec, opts.Prune, d)
}

// LiveObjects returns the app's managed live resources from the warm cache,
// prepared (stamp tracking, fill namespace) the same way Sync prepares the target
// so the live keys match. `ksync diff` reads it to learn the dev tags the cluster
// currently runs build images at (for build-tag carry-forward) before diffing.
func (e *Engine) LiveObjects(app, namespace string, resources []*unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
	target := StampTracking(app, resources)
	if namespace != "" {
		fillDefaultNamespace(target, namespace, e.clusterCache.IsNamespaced)
	}
	isManaged := appManaged(app)
	live, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
	if err != nil {
		return nil, fmt.Errorf("reading live state of %q: %w", app, err)
	}
	out := make([]*unstructured.Unstructured, 0, len(live))
	for _, o := range live {
		out = append(out, o)
	}
	return out, nil
}

// diffResources is the pure core of Diff: it classifies each reconciled
// target/live pair — target-only as a creation, live-only as a prune, both
// present as an update only when their content differs — masks secret values,
// and renders the YAML of each side. Split out so the classification rule is
// unit-testable without a cluster cache.
//
// Classification is structural (the pair's nil-ness), not DiffResult.Modified,
// which is false for a deletion-shaped Diff(nil, live).
func diffResources(rec sync.ReconciliationResult, prune bool, d *differ) ([]ResourceDiff, error) {
	var out []ResourceDiff
	for i := range rec.Target {
		t, l := rec.Target[i], rec.Live[i]
		if isHook(t) || isHook(l) {
			continue // applied by the hook machinery, not the normal resource pass
		}
		if t == nil && (!prune || pruningDisabled(l)) {
			// A prune candidate that prune is off for, or whose own sync-option opts
			// out of pruning (as sync's pruneObject skips it) — never deleted, so it
			// is not a diff.
			continue
		}
		// Compute the diff with the chosen strategy. Client-side classifies from the
		// UNMASKED objects so sync-fidelity rests only on the real data (a Secret's
		// values are masked for display below, not for classification). serverMasked
		// reports the one case where the values are ALREADY masked — a server-side
		// diff of an existing Secret — so the display-masking step is skipped only
		// then. Noise the rendered target never carries (managedFields, status,
		// resourceVersion, …) is stripped on both paths so Modified reflects only the
		// author's change.
		dr, serverMasked, err := d.classify(t, l)
		if err != nil {
			return nil, fmt.Errorf("diffing resource: %w", err)
		}
		var rd ResourceDiff
		switch {
		case t != nil && l == nil:
			rd = newResourceDiff(kube.GetResourceKey(t), DiffCreate)
		case t == nil && l != nil:
			rd = newResourceDiff(kube.GetResourceKey(l), DiffPrune)
		default:
			if !dr.Modified {
				continue // in sync
			}
			rd = newResourceDiff(kube.GetResourceKey(t), DiffUpdate)
		}
		// A Secret's displayed YAML is recomputed on masked copies so its (decodable)
		// values never reach the terminal. diff.Diff alone only base64-encodes
		// stringData (NormalizeSecret), it does not redact; HideSecretData does, but
		// hides every key under `data`, so it must touch Secrets only — a ConfigMap
		// (also a `data` map) keeps its values. HideSecretData maps distinct values to
		// distinct `+`-run lengths, so a value-only change still renders a (valueless)
		// hunk rather than vanishing. Skip this ONLY when the server-side path already
		// masked the values — which it does for an existing Secret but NOT for a
		// create or prune (those take gitops-engine's shallow, unmasked diff), so
		// serverMasked (not merely "server-side was used") is the right gate.
		display := dr
		if !serverMasked && (isSecret(t) || isSecret(l)) {
			mt, ml, err := diff.HideSecretData(t, l, nil)
			if err != nil {
				return nil, fmt.Errorf("masking secret data: %w", err)
			}
			if display, err = diff.Diff(normalizeForDiff(mt, d.stripLabel), normalizeForDiff(ml, d.stripLabel), diff.WithLogr(d.log)); err != nil {
				return nil, fmt.Errorf("diffing secret: %w", err)
			}
		}
		if rd.Before, err = jsonToYAML(display.NormalizedLive); err != nil {
			return nil, err
		}
		if rd.After, err = jsonToYAML(display.PredictedLive); err != nil {
			return nil, err
		}
		out = append(out, rd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sortKey() < out[j].sortKey() })
	return out, nil
}

// sortKey orders diffs for stable output. kube.ResourceKey.String has a pointer
// receiver, so bind the key to a local before formatting it.
func (d ResourceDiff) sortKey() string {
	k := d.key()
	return k.String()
}

func newResourceDiff(key kube.ResourceKey, typ DiffType) ResourceDiff {
	return ResourceDiff{Group: key.Group, Kind: key.Kind, Namespace: key.Namespace, Name: key.Name, Type: typ}
}

// isHook reports whether obj is a non-nil hook resource.
func isHook(obj *unstructured.Unstructured) bool {
	return obj != nil && hook.IsHook(obj)
}

// diffNoiseFields are the metadata paths the API server or ksync own, never the
// author: the rendered target carries none of them, so without stripping they
// dominate the diff (a full managedFields block, the whole status, the tracking
// label on every resource). Stripped from both sides before diffing.
var diffNoiseFields = [][]string{
	{"metadata", "managedFields"},
	{"metadata", "creationTimestamp"},
	{"metadata", "generation"},
	{"metadata", "resourceVersion"},
	{"metadata", "uid"},
	{"metadata", "selfLink"},
	{"status"},
}

// normalizeForDiff returns a deep copy of obj with the server-managed and
// ksync-bookkeeping fields stripped, so the diff reflects the author's intent
// rather than apply noise. nil-safe (a creation/prune has one nil side). See
// stripDiffNoise for the stripLabel contract.
func normalizeForDiff(obj *unstructured.Unstructured, stripLabel bool) *unstructured.Unstructured {
	if obj == nil {
		return nil
	}
	c := obj.DeepCopy()
	stripDiffNoise(c, stripLabel)
	return c
}

// stripDiffNoise removes the noise fields from obj in place. Shared by
// normalizeForDiff (client-side, on copies) and the server-side noiseNormalizer
// (which gitops-engine applies to the live and predicted-live objects directly).
//
// stripLabel removes the ksync tracking label, which differs by context: the diff
// *display* strips it so adopting an unmanaged resource — adding only that label —
// does not read as a change; the sync *apply-set decision* keeps it so adopting an
// otherwise-matching resource is seen as modified and applied (and thereby owned).
func stripDiffNoise(obj *unstructured.Unstructured, stripLabel bool) {
	for _, path := range diffNoiseFields {
		unstructured.RemoveNestedField(obj.Object, path...)
	}
	if stripLabel {
		unstructured.RemoveNestedField(obj.Object, "metadata", "labels", TrackingLabel)
		// StampTracking adds a labels map even to a resource that had none; once the
		// tracking label is stripped that can leave an empty map, which would itself
		// diff against a live object carrying no labels field. Drop the empty map.
		if labels, found, _ := unstructured.NestedMap(obj.Object, "metadata", "labels"); found && len(labels) == 0 {
			unstructured.RemoveNestedField(obj.Object, "metadata", "labels")
		}
	}
}

// pruningDisabled reports whether a live resource opts out of pruning via
// `argocd.argoproj.io/sync-options: Prune=false` — the annotation sync's own
// pruneObject honors by skipping the delete. Keeping diff in step avoids reporting
// a prune a sync would not perform.
func pruningDisabled(obj *unstructured.Unstructured) bool {
	return obj != nil && syncresource.HasAnnotationOption(obj, common.AnnotationSyncOptions, common.SyncOptionPrune+"="+common.SyncValueFalse)
}

// isSecret reports whether obj is a non-nil core/v1 Secret — the only kind whose
// values diff.HideSecretData should mask.
func isSecret(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	gvk := obj.GroupVersionKind()
	return gvk.Group == "" && gvk.Kind == "Secret"
}

// jsonToYAML converts one side of a DiffResult (JSON, "null" for an absent side)
// to YAML, returning "" for the absent side so a creation has no Before and a
// prune no After.
func jsonToYAML(b []byte) (string, error) {
	if len(b) == 0 || string(b) == "null" {
		return "", nil
	}
	y, err := yaml.JSONToYAML(b)
	if err != nil {
		return "", fmt.Errorf("rendering diff as YAML: %w", err)
	}
	return string(y), nil
}
