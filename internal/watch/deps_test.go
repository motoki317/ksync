package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDependencyRoots_ChartHomeOutsideRoot(t *testing.T) {
	tmp := t.TempDir()
	write(t, tmp, "app/kustomization.yaml", `
helmGlobals:
  chartHome: ../charts
helmCharts:
  - name: service
    releaseName: api-b
`)
	got, err := DependencyRoots(filepath.Join(tmp, "app"))
	if err != nil {
		t.Fatalf("DependencyRoots: %v", err)
	}
	if want := []string{filepath.Join(tmp, "charts")}; !reflect.DeepEqual(got, want) {
		t.Errorf("DependencyRoots = %v, want %v", got, want)
	}
}

func TestDependencyRoots_ChartHomeInsideRootIsNotADependency(t *testing.T) {
	tmp := t.TempDir()
	write(t, tmp, "app/kustomization.yaml", `
helmCharts:
  - name: service
    releaseName: api-b
`)
	got, err := DependencyRoots(filepath.Join(tmp, "app"))
	if err != nil {
		t.Fatalf("DependencyRoots: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DependencyRoots = %v, want none (default chartHome lives inside the app dir)", got)
	}
}

func TestDependencyRoots_PlainKustomizationHasNone(t *testing.T) {
	tmp := t.TempDir()
	write(t, tmp, "app/kustomization.yaml", "resources:\n  - configmap.yaml\n")
	got, err := DependencyRoots(filepath.Join(tmp, "app"))
	if err != nil {
		t.Fatalf("DependencyRoots: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DependencyRoots = %v, want none", got)
	}
}

func TestDependencyRoots_ResourceRefsEscapingRootAreFollowedTransitively(t *testing.T) {
	tmp := t.TempDir()
	write(t, tmp, "app/kustomization.yaml", "resources:\n  - ../base\n")
	write(t, tmp, "base/kustomization.yaml", "resources:\n  - ../common/thing.yaml\n")
	write(t, tmp, "common/thing.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: thing\n")
	got, err := DependencyRoots(filepath.Join(tmp, "app"))
	if err != nil {
		t.Fatalf("DependencyRoots: %v", err)
	}
	want := []string{filepath.Join(tmp, "base"), filepath.Join(tmp, "common", "thing.yaml")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DependencyRoots = %v, want %v", got, want)
	}
}

func TestDependencyRoots_NestedKustomizationInsideRootCanEscape(t *testing.T) {
	// A subdirectory of the app is not itself a dependency, but its
	// kustomization may reference paths outside the app dir.
	tmp := t.TempDir()
	write(t, tmp, "app/kustomization.yaml", "resources:\n  - sub\n")
	write(t, tmp, "app/sub/kustomization.yaml", `
helmGlobals:
  chartHome: ../../charts
helmCharts:
  - name: service
    releaseName: api-b
`)
	got, err := DependencyRoots(filepath.Join(tmp, "app"))
	if err != nil {
		t.Fatalf("DependencyRoots: %v", err)
	}
	if want := []string{filepath.Join(tmp, "charts")}; !reflect.DeepEqual(got, want) {
		t.Errorf("DependencyRoots = %v, want %v", got, want)
	}
}

func TestDependencyRoots_RemoteRefsAreIgnored(t *testing.T) {
	tmp := t.TempDir()
	write(t, tmp, "app/kustomization.yaml", `
resources:
  - github.com/org/repo//manifests?ref=v1.0.0
  - https://example.com/manifest.yaml
`)
	got, err := DependencyRoots(filepath.Join(tmp, "app"))
	if err != nil {
		t.Fatalf("DependencyRoots: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DependencyRoots = %v, want none (remote refs are not watchable)", got)
	}
}

func TestDependencyRoots_CyclicReferencesTerminate(t *testing.T) {
	tmp := t.TempDir()
	write(t, tmp, "a/kustomization.yaml", "resources:\n  - ../b\n")
	write(t, tmp, "b/kustomization.yaml", "resources:\n  - ../a\n")
	got, err := DependencyRoots(filepath.Join(tmp, "a"))
	if err != nil {
		t.Fatalf("DependencyRoots: %v", err)
	}
	// The app's own dir is never a dependency of itself.
	if want := []string{filepath.Join(tmp, "b")}; !reflect.DeepEqual(got, want) {
		t.Errorf("DependencyRoots = %v, want %v", got, want)
	}
}

func TestDependencyRoots_HelmValuesFilesOutsideRoot(t *testing.T) {
	tmp := t.TempDir()
	write(t, tmp, "app/kustomization.yaml", `
helmCharts:
  - name: service
    releaseName: api-b
    valuesFile: ../values/api-b.yaml
    additionalValuesFiles:
      - ../values/common.yaml
`)
	got, err := DependencyRoots(filepath.Join(tmp, "app"))
	if err != nil {
		t.Fatalf("DependencyRoots: %v", err)
	}
	want := []string{
		filepath.Join(tmp, "values", "api-b.yaml"),
		filepath.Join(tmp, "values", "common.yaml"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DependencyRoots = %v, want %v", got, want)
	}
}

func TestDependencyRoots_MissingKustomizationIsAnError(t *testing.T) {
	tmp := t.TempDir()
	if _, err := DependencyRoots(filepath.Join(tmp, "app")); err == nil {
		t.Fatal("DependencyRoots succeeded on a directory without a kustomization file")
	}
}

func write(t *testing.T, base, rel, content string) {
	t.Helper()
	path := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
