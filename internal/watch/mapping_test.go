package watch

import (
	"reflect"
	"strings"
	"testing"
)

func TestMapping_PathInsideAppRootAffectsThatApp(t *testing.T) {
	m := NewMapping([]AppRoots{
		{App: "api-b", Roots: []string{"/repo/apps/api-b"}},
		{App: "shop", Roots: []string{"/repo/apps/shop"}},
	})
	got := m.AffectedBy("/repo/apps/api-b/values.yaml")
	if want := []string{"api-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AffectedBy = %v, want %v", got, want)
	}
}

func TestMapping_SharedRootAffectsAllAppsInDeclarationOrder(t *testing.T) {
	m := NewMapping([]AppRoots{
		{App: "shop", Roots: []string{"/repo/apps/shop", "/repo/charts"}},
		{App: "api-b", Roots: []string{"/repo/apps/api-b", "/repo/charts"}},
	})
	got := m.AffectedBy("/repo/charts/service/templates/deployment.yaml")
	if want := []string{"shop", "api-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AffectedBy = %v, want %v", got, want)
	}
}

func TestMapping_SiblingDirSharingPrefixIsNotAffected(t *testing.T) {
	// "/repo/apps/api-b2" starts with the string "/repo/apps/api-b"; matching
	// must respect path boundaries, not raw prefixes.
	m := NewMapping([]AppRoots{
		{App: "api-b", Roots: []string{"/repo/apps/api-b"}},
	})
	if got := m.AffectedBy("/repo/apps/api-b2/values.yaml"); len(got) != 0 {
		t.Errorf("AffectedBy = %v, want none", got)
	}
}

func TestMapping_RootPathItselfAffectsTheApp(t *testing.T) {
	m := NewMapping([]AppRoots{
		{App: "api-b", Roots: []string{"/repo/apps/api-b", "/repo/values/api-b.yaml"}},
	})
	// A file root matches by equality (e.g. a shared values file).
	if got := m.AffectedBy("/repo/values/api-b.yaml"); len(got) != 1 || got[0] != "api-b" {
		t.Errorf("AffectedBy = %v, want [api-b]", got)
	}
}

func TestMapping_UnrelatedPathAffectsNothing(t *testing.T) {
	m := NewMapping([]AppRoots{
		{App: "api-b", Roots: []string{"/repo/apps/api-b"}},
	})
	if got := m.AffectedBy("/repo/README.md"); len(got) != 0 {
		t.Errorf("AffectedBy = %v, want none", got)
	}
}

func TestMapping_AppWithOverlappingRootsIsReportedOnce(t *testing.T) {
	m := NewMapping([]AppRoots{
		{App: "api-b", Roots: []string{"/repo/apps/api-b", "/repo/apps/api-b/extra"}},
	})
	if got := m.AffectedBy("/repo/apps/api-b/extra/file.yaml"); len(got) != 1 {
		t.Errorf("AffectedBy = %v, want exactly one entry", got)
	}
}

func TestMapping_IgnorePredicateFiltersPaths(t *testing.T) {
	m := NewMapping([]AppRoots{
		{
			App:    "api-b-build",
			Roots:  []string{"/src/api-b"},
			Ignore: func(path string) bool { return strings.HasSuffix(path, ".md") },
		},
		{App: "api-b", Roots: []string{"/src/api-b/manifests"}},
	})
	if got := m.AffectedBy("/src/api-b/README.md"); len(got) != 0 {
		t.Errorf("AffectedBy = %v, want none (ignored by the entry's predicate)", got)
	}
	// The predicate binds to its own entry only: another entry watching an
	// overlapping root still fires.
	if got := m.AffectedBy("/src/api-b/manifests/notes.md"); !reflect.DeepEqual(got, []string{"api-b"}) {
		t.Errorf("AffectedBy = %v, want [api-b]", got)
	}
	if got := m.AffectedBy("/src/api-b/main.go"); !reflect.DeepEqual(got, []string{"api-b-build"}) {
		t.Errorf("AffectedBy = %v, want [api-b-build]", got)
	}
}
