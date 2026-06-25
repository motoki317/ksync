package engine

import (
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The tracking label is treated differently by context: the diff display
// (stripLabel=true) hides a label-only adoption as noise, but the sync apply-set
// decision (stripLabel=false) must see an unlabeled-but-otherwise-matching live
// resource as modified — else it is never applied, never gets the label, and
// prune never manages it. Same content, live missing only the tracking label.
func TestClassify_TrackingLabelByContext(t *testing.T) {
	target := configMap("team-a", "cm")
	target.SetLabels(map[string]string{TrackingLabel: "shop"})
	live := configMap("team-a", "cm") // identical content, but unadopted (no tracking label)

	apply := &differ{stripLabel: false, log: logr.Discard()} // sync apply-set decision
	dr, _, err := apply.classify(target, live)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if !dr.Modified {
		t.Error("sync must see an unlabeled live as modified, so the resource is applied and adopted")
	}

	display := &differ{stripLabel: true, log: logr.Discard()} // diff display
	dr, _, err = display.classify(target, live)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if dr.Modified {
		t.Error("diff display should hide a label-only adoption as noise")
	}
}

// diffArray must keep each pair's result aligned to the input order and OR the
// per-pair Modified into the list's Modified — the contract
// WithResourceModificationChecker relies on to apply only out-of-sync resources.
// Exercised with the client-side strategy (the server-side path dry-runs against
// a live apiserver and is validated end-to-end, not in a unit test).
func TestDiffArray_AlignsAndAggregates(t *testing.T) {
	configs := []*unstructured.Unstructured{
		cmWithData("team-a", "a", map[string]string{"k": "v2"}),   // differs from live
		cmWithData("team-a", "b", map[string]string{"k": "same"}), // matches live
	}
	lives := []*unstructured.Unstructured{
		cmWithData("team-a", "a", map[string]string{"k": "v1"}),
		cmWithData("team-a", "b", map[string]string{"k": "same"}),
	}

	drl, err := clientDiffer().diffArray(configs, lives)
	if err != nil {
		t.Fatalf("diffArray: %v", err)
	}
	if len(drl.Diffs) != 2 {
		t.Fatalf("got %d diffs, want 2 (one per pair, in order)", len(drl.Diffs))
	}
	if !drl.Diffs[0].Modified {
		t.Error("pair 0 (v1→v2) should be modified")
	}
	if drl.Diffs[1].Modified {
		t.Error("pair 1 (unchanged) should not be modified")
	}
	if !drl.Modified {
		t.Error("list Modified should be true when any pair is modified")
	}
}
