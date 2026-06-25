package engine

import (
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func cmWithData(ns, name string, data map[string]string) *unstructured.Unstructured {
	d := make(map[string]any, len(data))
	for k, v := range data {
		d[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"data":       d,
	}}
}

func secretWithData(ns, name string, data map[string]string) *unstructured.Unstructured {
	d := make(map[string]any, len(data))
	for k, v := range data {
		d[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"data":       d,
	}}
}

// reconciled builds a ReconciliationResult from aligned target/live pairs — the
// shape sync.Reconcile produces, so diffResources can be exercised without a
// cluster cache (a nil target is a prune candidate, a nil live a creation).
func reconciled(pairs ...[2]*unstructured.Unstructured) sync.ReconciliationResult {
	var r sync.ReconciliationResult
	for _, p := range pairs {
		r.Target = append(r.Target, p[0])
		r.Live = append(r.Live, p[1])
	}
	return r
}

func pair(target, live *unstructured.Unstructured) [2]*unstructured.Unstructured {
	return [2]*unstructured.Unstructured{target, live}
}

// clientDiffer is the in-process diff strategy with the diff-command's display
// contract (stripLabel=true) — the only strategy diffResources can run without a
// cluster, so every pure classification test uses it. The server-side strategy is
// exercised live (it dry-runs against the apiserver).
func clientDiffer() *differ {
	return &differ{serverSide: false, stripLabel: true, log: logr.Discard()}
}

// diffResources is the read-only preview's core: each reconciled pair is
// classified structurally — target-only is a creation, live-only a prune, both
// present an update only when their content differs. Hooks and unchanged
// resources never appear.
func TestDiffResources_ClassifiesByReconciledPair(t *testing.T) {
	create := pair(configMap("team-a", "new-cm"), nil)
	update := pair(cmWithData("team-a", "settings", map[string]string{"k": "v2"}),
		cmWithData("team-a", "settings", map[string]string{"k": "v1"}))
	unchanged := pair(cmWithData("team-a", "stable", map[string]string{"k": "same"}),
		cmWithData("team-a", "stable", map[string]string{"k": "same"}))
	prune := pair(nil, configMap("team-a", "old-cm"))

	got, err := diffResources(reconciled(create, update, unchanged, prune), true, clientDiffer())
	if err != nil {
		t.Fatalf("diffResources: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d diffs, want 3 (create, update, prune; unchanged excluded): %+v", len(got), got)
	}
	byName := map[string]ResourceDiff{}
	for _, d := range got {
		byName[d.Name] = d
	}
	if d := byName["new-cm"]; d.Type != DiffCreate || d.Before != "" || !strings.Contains(d.After, "new-cm") {
		t.Errorf("new-cm = %+v, want create with empty Before and a populated After", d)
	}
	if d := byName["settings"]; d.Type != DiffUpdate || !strings.Contains(d.Before, "v1") || !strings.Contains(d.After, "v2") {
		t.Errorf("settings = %+v, want update from v1 to v2", d)
	}
	if d := byName["old-cm"]; d.Type != DiffPrune || d.After != "" || !strings.Contains(d.Before, "old-cm") {
		t.Errorf("old-cm = %+v, want prune with a populated Before and empty After", d)
	}
	if _, ok := byName["stable"]; ok {
		t.Errorf("unchanged resource must not appear in the diff: %+v", byName["stable"])
	}
}

// Prune candidates show only when prune is enabled — `--prune=false` mirrors a
// sync that would not delete them, so the preview must not either.
func TestDiffResources_PruneDisabledHidesPruneCandidates(t *testing.T) {
	got, err := diffResources(reconciled(pair(nil, configMap("team-a", "old-cm"))), false, clientDiffer())
	if err != nil {
		t.Fatalf("diffResources: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none (prune disabled)", got)
	}
}

// A live resource annotated Prune=false is one sync's pruneObject skips, so the
// diff must not report it as a prune even with prune enabled.
func TestDiffResources_RespectsPruneFalseOption(t *testing.T) {
	keep := configMap("team-a", "keep-me")
	keep.SetAnnotations(map[string]string{"argocd.argoproj.io/sync-options": "Prune=false"})
	got, err := diffResources(reconciled(pair(nil, keep)), true, clientDiffer())
	if err != nil {
		t.Fatalf("diffResources: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none (Prune=false opts the resource out of pruning)", got)
	}
}

// A hook is applied by sync's hook machinery, not the normal resource pass, and a
// lingering live hook Job from a prior run would otherwise mis-report as a prune.
func TestDiffResources_ExcludesHooks(t *testing.T) {
	liveHook := hookPod("team-a", "postsync-job", "Succeeded")
	targetHook := pair(hookPod("team-a", "presync-job", "Succeeded"), nil)
	got, err := diffResources(reconciled(targetHook, pair(nil, liveHook)), true, clientDiffer())
	if err != nil {
		t.Fatalf("diffResources: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none (hooks excluded from the diff)", got)
	}
}

// Server-managed and ksync-bookkeeping fields (managedFields, status, the
// tracking label) the rendered target never carries must not show as diffs.
// Here only those fields differ, so the resource is reported as in sync.
func TestDiffResources_StripsApplyNoise(t *testing.T) {
	target := cmWithData("team-a", "settings", map[string]string{"k": "v"})
	live := cmWithData("team-a", "settings", map[string]string{"k": "v"})
	live.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: "Apply"}})
	live.SetResourceVersion("12345")
	live.SetLabels(map[string]string{TrackingLabel: "settings"}) // live carries the tracking label, target does not
	if err := unstructured.SetNestedMap(live.Object, map[string]any{"phase": "Active"}, "status"); err != nil {
		t.Fatalf("set status: %v", err)
	}

	got, err := diffResources(reconciled(pair(target, live)), true, clientDiffer())
	if err != nil {
		t.Fatalf("diffResources: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none (only apply-noise fields differ)", got)
	}
}

// Secret values must never reach the terminal: diff.Diff alone leaves base64
// plaintext, so diffResources masks via HideSecretData before rendering.
func TestDiffResources_MasksSecretValues(t *testing.T) {
	secret := secretWithData("team-a", "creds", map[string]string{"password": "c3VwZXItc2VjcmV0"}) // base64("super-secret")
	got, err := diffResources(reconciled(pair(secret, nil)), true, clientDiffer())
	if err != nil {
		t.Fatalf("diffResources: %v", err)
	}
	if len(got) != 1 || got[0].Type != DiffCreate {
		t.Fatalf("got %+v, want one create", got)
	}
	if strings.Contains(got[0].After, "c3VwZXItc2VjcmV0") {
		t.Errorf("secret value leaked into the diff: %s", got[0].After)
	}
	if !strings.Contains(got[0].After, "++++++++") {
		t.Errorf("secret value not masked; After = %s", got[0].After)
	}
}

// A Secret whose value changed (same keys) must classify as an update, and the
// rendered sides must stay masked yet differ. Classification runs on the unmasked
// objects so it rests on the real data, not on HideSecretData's display masking;
// the masked sides still differ here because HideSecretData gives distinct values
// distinct-length masks, so the change shows without exposing the value.
func TestDiffResources_SecretValueOnlyChangeIsUpdate(t *testing.T) {
	before := secretWithData("team-a", "creds", map[string]string{"password": "b2xk"})        // base64("old")
	after := secretWithData("team-a", "creds", map[string]string{"password": "bmV3LXZhbHVl"}) // base64("new-value")
	got, err := diffResources(reconciled(pair(after, before)), true, clientDiffer())
	if err != nil {
		t.Fatalf("diffResources: %v", err)
	}
	if len(got) != 1 || got[0].Type != DiffUpdate {
		t.Fatalf("got %+v, want one update (value changed)", got)
	}
	for _, v := range []string{"b2xk", "bmV3LXZhbHVl"} {
		if strings.Contains(got[0].Before, v) || strings.Contains(got[0].After, v) {
			t.Errorf("secret value %q leaked into the diff: before=%q after=%q", v, got[0].Before, got[0].After)
		}
	}
	if got[0].Before == got[0].After {
		t.Errorf("masked sides are identical, so the value change would not render: %q", got[0].Before)
	}
	if !strings.Contains(got[0].Before, "++++++++") || !strings.Contains(got[0].After, "++++++++") {
		t.Errorf("expected masked values on both sides: before=%q after=%q", got[0].Before, got[0].After)
	}
}
