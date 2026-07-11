package render

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/kustomize/api/filters/patchjson6902"
	"sigs.k8s.io/kustomize/api/resource"

	"github.com/motoki317/ksync/internal/config"
)

// NewVarLookup returns the variable resolver for patch ${VAR} expansion: the
// process environment, plus the built-in ${KSYNC_WORKDIR} = workdir (the config
// file's directory). An undefined variable returns ok=false, which fails the
// expansion closed. KSYNC_WORKDIR shadows any same-named process variable, so
// the anchor is always the config directory regardless of the environment.
func NewVarLookup(workdir string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if name == "KSYNC_WORKDIR" {
			return workdir, true
		}
		return os.LookupEnv(name)
	}
}

// ApplyPatches applies each config patch to this result's rendered objects,
// before image injection and apply. For each patch it resolves ${VAR} in the op
// `value` strings via lookup (an undefined variable with no ${VAR:-default}
// fallback is an error), finds the one
// rendered object the target names by literal GVK+name (and namespace when the
// target pins one) — failing closed if zero or several match — and applies the
// RFC 6902 ops to it (a failed `test` op or a missing path fails the whole
// patch). It mutates the resMap and refreshes Objects together, so
// Result.YAML() and Result.Objects stay consistent. On any error the caller
// must discard this Result: an earlier patch in the list may already have
// mutated it (RFC 6902 is atomic per object, not across the list).
func (res *Result) ApplyPatches(patches []config.Patch, lookup func(string) (string, bool)) error {
	if len(patches) == 0 {
		return nil
	}
	for _, p := range patches {
		body, err := expandPatchValues(p.Patch, lookup)
		if err != nil {
			return fmt.Errorf("patch %s: %w", targetString(p.Target), err)
		}
		matches := matchTarget(res.resMap.Resources(), p.Target)
		if len(matches) != 1 {
			return fmt.Errorf("patch target %s matched %d rendered objects, need exactly 1", targetString(p.Target), len(matches))
		}
		if err := matches[0].ApplyFilter(patchjson6902.Filter{Patch: body}); err != nil {
			return fmt.Errorf("applying patch to %s: %w", targetString(p.Target), err)
		}
	}
	objs, err := objectsFromResMap(res.resMap)
	if err != nil {
		return err
	}
	res.Objects = objs
	return nil
}

// matchTarget returns the resources whose kind and name — and group, version,
// and namespace when the target sets them — equal the target literally. This is
// deliberately not kustomize's Selector, which matches IDs by regex: a dotted
// group like "networking.k8s.io" must match as an exact string, not a pattern.
// An empty target Group, Version, or Namespace matches any.
func matchTarget(rs []*resource.Resource, t config.PatchTarget) []*resource.Resource {
	var out []*resource.Resource
	for _, r := range rs {
		gvk := r.GetGvk()
		if t.Kind != gvk.Kind || t.Name != r.GetName() {
			continue
		}
		if t.Group != "" && t.Group != gvk.Group {
			continue
		}
		if t.Version != "" && t.Version != gvk.Version {
			continue
		}
		if t.Namespace != "" && t.Namespace != r.GetNamespace() {
			continue
		}
		out = append(out, r)
	}
	return out
}

// targetString renders a patch target for error messages, e.g.
// "example.com/v1/Pipeline/data-job in shop".
func targetString(t config.PatchTarget) string {
	id := t.Kind
	if gv := strings.Trim(t.Group+"/"+t.Version, "/"); gv != "" {
		id = gv + "/" + t.Kind
	}
	id += "/" + t.Name
	if t.Namespace != "" {
		id += " in " + t.Namespace
	}
	return id
}

// expandPatchValues parses the inline patch and expands ${VAR} references in the
// op `value` strings only — never `path`, `from`, or `op` — returning a JSON
// patch string. Restricting expansion to values keeps a variable from altering
// the patch structure or a JSON pointer, and recursing into nested values
// covers a value that is an object or array.
func expandPatchValues(patch string, lookup func(string) (string, bool)) (string, error) {
	ops, err := config.DecodePatchOps(patch)
	if err != nil {
		return "", err
	}
	for _, op := range ops {
		raw, ok := op["value"]
		if !ok {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", err
		}
		ev, err := expandAny(v, lookup)
		if err != nil {
			return "", err
		}
		nb, err := json.Marshal(ev)
		if err != nil {
			return "", err
		}
		op["value"] = nb
	}
	out, err := json.Marshal(ops)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// expandAny expands ${VAR} in every string within a decoded JSON value,
// recursing through arrays and objects and leaving non-strings unchanged.
func expandAny(v any, lookup func(string) (string, bool)) (any, error) {
	switch t := v.(type) {
	case string:
		return expandVars(t, lookup)
	case []any:
		for i := range t {
			ev, err := expandAny(t[i], lookup)
			if err != nil {
				return nil, err
			}
			t[i] = ev
		}
		return t, nil
	case map[string]any:
		for k := range t {
			ev, err := expandAny(t[k], lookup)
			if err != nil {
				return nil, err
			}
			t[k] = ev
		}
		return t, nil
	default:
		return v, nil
	}
}

// expandVars replaces ${NAME} with lookup(NAME), ${NAME:-default} with the
// variable's value or, when it is unset or empty, the literal default, and $$
// with a literal $. A bare ${NAME} whose variable is undefined is an error
// (fail-closed); the ${NAME:-default} form is the explicit opt-out that supplies
// a fallback instead. The default follows POSIX ${parameter:-word} colon
// semantics (substituted when the variable is unset OR empty), so
// ${GOOGLE_MAPS_API_KEY:-} yields "" when that variable is absent — matching a
// helmfile `env "" ` default without breaking the fail-closed rule. The default
// is taken literally: it is not itself re-expanded, and the first "}" ends the
// form, so a nested ${...} inside a default is not supported. Only the colon
// form ":-" is recognized (no unset-only "-"). Any other $ is kept literally, so
// the grammar is exactly "${NAME}", "${NAME:-default}", and "$$" — narrower than
// shell expansion, which would also substitute a bare $word and special forms,
// surprising a value that legitimately contains a $.
func expandVars(s string, lookup func(string) (string, bool)) (string, error) {
	if !strings.ContainsRune(s, '$') {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		switch {
		case i+1 < len(s) && s[i+1] == '$':
			b.WriteByte('$')
			i += 2
		case i+1 < len(s) && s[i+1] == '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated ${...} in %q", s)
			}
			name, def, hasDefault := strings.Cut(s[i+2:i+2+end], ":-")
			if name == "" {
				return "", fmt.Errorf("empty variable name in ${%s}", s[i+2:i+2+end])
			}
			val, ok := lookup(name)
			switch {
			case ok && val != "":
				b.WriteString(val)
			case hasDefault:
				b.WriteString(def)
			case !ok:
				return "", fmt.Errorf("undefined variable ${%s}", name)
			default:
				b.WriteString(val) // set but empty, no default: preserve prior behavior
			}
			i += 2 + end + 1
		default:
			b.WriteByte('$')
			i++
		}
	}
	return b.String(), nil
}
