package config

import (
	"strings"
	"testing"
)

func TestParse_ValidPatch(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    patches:
      - target: { group: apps, version: v1, kind: Deployment, name: api-b, namespace: team-a }
        patch: |
          - op: replace
            path: /spec/template/spec/volumes/0/hostPath/path
            value: ${KSYNC_WORKDIR}/secrets
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Dir() != "/cfg" {
		t.Errorf("Dir() = %q, want /cfg", cfg.Dir())
	}
	ps := cfg.Apps[0].Patches
	if len(ps) != 1 {
		t.Fatalf("len(Patches) = %d, want 1", len(ps))
	}
	if ps[0].Target.Kind != "Deployment" || ps[0].Target.Name != "api-b" {
		t.Errorf("Target = %+v, want Deployment/api-b", ps[0].Target)
	}
	if !strings.Contains(ps[0].Patch, "${KSYNC_WORKDIR}/secrets") {
		t.Errorf("Patch body not preserved: %q", ps[0].Patch)
	}
}

// value: null is valid for replace/add/test, so the validator must accept a
// present-but-null value (distinct from a missing one).
func TestParse_PatchValueNull(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    patches:
      - target: { kind: ConfigMap, name: c }
        patch: |
          - op: replace
            path: /data
            value: null
`
	if _, err := Parse([]byte(yml), "/cfg"); err != nil {
		t.Fatalf("Parse rejected value: null: %v", err)
	}
}

func TestParse_InvalidPatches(t *testing.T) {
	cases := []struct {
		name  string
		patch string // the YAML under patches:
		want  string
	}{
		{
			name: "missing kind",
			patch: `      - target: { name: x }
        patch: |
          - op: replace
            path: /a
            value: b`,
			want: "target.kind is required",
		},
		{
			name: "missing name",
			patch: `      - target: { kind: Deployment }
        patch: |
          - op: replace
            path: /a
            value: b`,
			want: "target.name is required",
		},
		{
			name: "unknown op",
			patch: `      - target: { kind: Deployment, name: x }
        patch: |
          - op: frobnicate
            path: /a
            value: b`,
			want: `unknown op "frobnicate"`,
		},
		{
			name: "add without value",
			patch: `      - target: { kind: Deployment, name: x }
        patch: |
          - op: add
            path: /a`,
			want: `"add" requires a value`,
		},
		{
			name: "path not a pointer",
			patch: `      - target: { kind: Deployment, name: x }
        patch: |
          - op: replace
            path: spec/replicas
            value: 1`,
			want: "is not a JSON pointer",
		},
		{
			name: "patch not a list",
			patch: `      - target: { kind: Deployment, name: x }
        patch: "not a patch"`,
			want: "must be a list of RFC 6902 operations",
		},
		{
			name: "empty patch",
			patch: `      - target: { kind: Deployment, name: x }
        patch: "[]"`,
			want: "patch has no operations",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			yml := "allowedContexts: [docker-desktop]\napps:\n  - path: apps/api-b\n    patches:\n" + c.patch + "\n"
			_, err := Parse([]byte(yml), "/cfg")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
