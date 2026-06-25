package render

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/config"
)

func renderPatchFixture(t *testing.T) *Result {
	t.Helper()
	res, err := New(Options{}).Render("testdata/patch")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return res
}

func constLookup(m map[string]string) func(string) (string, bool) {
	return func(n string) (string, bool) { v, ok := m[n]; return v, ok }
}

// cacheHostPath reads the cache Deployment's cache-vol hostPath from the rendered
// Objects slice — the one sync applies, so it is what must change.
func cacheHostPath(t *testing.T, res *Result) string {
	t.Helper()
	for _, o := range res.Objects {
		if o.GetKind() != "Deployment" || o.GetName() != "cache" {
			continue
		}
		vols, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "volumes")
		for _, v := range vols {
			m, _ := v.(map[string]any)
			if m["name"] == "cache-vol" {
				hp, _ := m["hostPath"].(map[string]any)
				return hp["path"].(string)
			}
		}
	}
	t.Fatal("cache cache-vol hostPath not found")
	return ""
}

func cacheAppTarget() config.PatchTarget {
	return config.PatchTarget{Group: "apps", Version: "v1", Kind: "Deployment", Name: "cache", Namespace: "team-a"}
}

// A patch rewrites the targeted hostPath, expanding ${KSYNC_WORKDIR}, and the
// change shows in both Objects and YAML() — the two must never drift.
func TestApplyPatches_ReplaceHostPathWithVar(t *testing.T) {
	res := renderPatchFixture(t)
	patches := []config.Patch{{
		Target: cacheAppTarget(),
		Patch: `- op: replace
  path: /spec/template/spec/volumes/0/hostPath/path
  value: ${KSYNC_WORKDIR}/.cache/shop`,
	}}
	if err := res.ApplyPatches(patches, constLookup(map[string]string{"KSYNC_WORKDIR": "/work"})); err != nil {
		t.Fatalf("ApplyPatches: %v", err)
	}
	if got := cacheHostPath(t, res); got != "/work/.cache/shop" {
		t.Errorf("hostPath = %q, want /work/.cache/shop", got)
	}
	yml, err := res.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(yml), "/work/.cache/shop") {
		t.Errorf("YAML() does not reflect the patch:\n%s", yml)
	}
}

// Any environment variable can be named, and $$ yields a literal $.
func TestApplyPatches_EnvVarAndDollarEscape(t *testing.T) {
	res := renderPatchFixture(t)
	patches := []config.Patch{{
		Target: cacheAppTarget(),
		Patch: `- op: replace
  path: /spec/template/spec/volumes/0/hostPath/path
  value: ${HOME}/cache$$raw`,
	}}
	if err := res.ApplyPatches(patches, constLookup(map[string]string{"HOME": "/root"})); err != nil {
		t.Fatalf("ApplyPatches: %v", err)
	}
	if got := cacheHostPath(t, res); got != "/root/cache$raw" {
		t.Errorf("hostPath = %q, want /root/cache$raw", got)
	}
}

// An undefined variable fails the patch closed.
func TestApplyPatches_UndefinedVarFails(t *testing.T) {
	res := renderPatchFixture(t)
	patches := []config.Patch{{
		Target: cacheAppTarget(),
		Patch: `- op: replace
  path: /spec/template/spec/volumes/0/hostPath/path
  value: ${NOT_SET}/x`,
	}}
	err := res.ApplyPatches(patches, constLookup(nil))
	if err == nil || !strings.Contains(err.Error(), "NOT_SET") {
		t.Fatalf("want undefined-variable error naming NOT_SET, got %v", err)
	}
}

// A failed `test` op aborts the patch (RFC 6902 atomicity) and leaves the
// object unchanged.
func TestApplyPatches_TestOpFailsClosed(t *testing.T) {
	res := renderPatchFixture(t)
	patches := []config.Patch{{
		Target: cacheAppTarget(),
		Patch: `- op: test
  path: /spec/template/spec/volumes/0/name
  value: wrong-name
- op: replace
  path: /spec/template/spec/volumes/0/hostPath/path
  value: /should-not-apply`,
	}}
	if err := res.ApplyPatches(patches, constLookup(nil)); err == nil {
		t.Fatal("want error from failed test op, got nil")
	}
	if got := cacheHostPath(t, res); got != "/placeholder" {
		t.Errorf("hostPath = %q, want /placeholder unchanged after a failed test op", got)
	}
}

// A passing `test` op lets the following ops apply.
func TestApplyPatches_TestOpPasses(t *testing.T) {
	res := renderPatchFixture(t)
	patches := []config.Patch{{
		Target: cacheAppTarget(),
		Patch: `- op: test
  path: /spec/template/spec/volumes/0/name
  value: cache-vol
- op: replace
  path: /spec/template/spec/volumes/0/hostPath/path
  value: /applied`,
	}}
	if err := res.ApplyPatches(patches, constLookup(nil)); err != nil {
		t.Fatalf("ApplyPatches: %v", err)
	}
	if got := cacheHostPath(t, res); got != "/applied" {
		t.Errorf("hostPath = %q, want /applied", got)
	}
}

// A target matching several objects (twin exists in two namespaces, no
// namespace pinned) is a fail-closed error, not a silent multi-apply.
func TestApplyPatches_MultiMatchFails(t *testing.T) {
	res := renderPatchFixture(t)
	patches := []config.Patch{{
		Target: config.PatchTarget{Kind: "Deployment", Name: "twin"},
		Patch: `- op: replace
  path: /spec/replicas
  value: 2`,
	}}
	err := res.ApplyPatches(patches, constLookup(nil))
	if err == nil || !strings.Contains(err.Error(), "matched 2") {
		t.Fatalf("want multi-match error, got %v", err)
	}
}

// A target matching nothing is also fatal.
func TestApplyPatches_ZeroMatchFails(t *testing.T) {
	res := renderPatchFixture(t)
	patches := []config.Patch{{
		Target: config.PatchTarget{Kind: "Deployment", Name: "ghost"},
		Patch: `- op: replace
  path: /spec/replicas
  value: 2`,
	}}
	err := res.ApplyPatches(patches, constLookup(nil))
	if err == nil || !strings.Contains(err.Error(), "matched 0") {
		t.Fatalf("want zero-match error, got %v", err)
	}
}

// Pinning the namespace selects one of two same-named objects.
func TestApplyPatches_NamespacePinsMatch(t *testing.T) {
	res := renderPatchFixture(t)
	patches := []config.Patch{{
		Target: config.PatchTarget{Kind: "Deployment", Name: "twin", Namespace: "b"},
		Patch: `- op: add
  path: /metadata/labels
  value:
    picked: "yes"`,
	}}
	if err := res.ApplyPatches(patches, constLookup(nil)); err != nil {
		t.Fatalf("ApplyPatches: %v", err)
	}
	for _, o := range res.Objects {
		if o.GetKind() == "Deployment" && o.GetName() == "twin" {
			labels := o.GetLabels()
			if o.GetNamespace() == "b" && labels["picked"] != "yes" {
				t.Errorf("twin in ns b was not patched")
			}
			if o.GetNamespace() == "a" && labels["picked"] == "yes" {
				t.Errorf("twin in ns a was patched but should not have been")
			}
		}
	}
}

func TestExpandVars(t *testing.T) {
	lookup := constLookup(map[string]string{"A": "x", "B": "/b/dir"})
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"${A}", "x", false},
		{"${B}/sub", "/b/dir/sub", false},
		{"plain", "plain", false},
		{"$$literal", "$literal", false},
		{"a$b", "a$b", false}, // lone $ not starting ${ or $$ stays literal
		{"${MISSING}", "", true},
		{"${A", "", true}, // unterminated
		{"${}", "", true}, // empty name
	}
	for _, c := range cases {
		got, err := expandVars(c.in, lookup)
		if c.wantErr {
			if err == nil {
				t.Errorf("expandVars(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("expandVars(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}
