package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"
)

// DependencyRoots returns the local paths outside appDir that the
// kustomization at appDir reads at render time: a chartHome outside the root,
// resource/component references escaping it (followed transitively through
// referenced kustomizations), and helm values files outside it. These are the
// extra paths a watcher must observe to know when the app needs re-rendering.
//
// Traversal through referenced kustomizations is best-effort — a broken
// reference surfaces at render time with a better error. Only an unreadable
// top-level kustomization is an error here.
func DependencyRoots(appDir string) ([]string, error) {
	appDir = filepath.Clean(appDir)
	var roots []string
	seen := map[string]bool{}
	addRoot := func(p string) {
		// The app dir and anything inside it are watched anyway.
		if p == appDir || isBelow(appDir, p) || seen[p] {
			return
		}
		seen[p] = true
		roots = append(roots, p)
	}

	visited := map[string]bool{}
	var walk func(dir string) error
	walk = func(dir string) error {
		if visited[dir] {
			return nil
		}
		visited[dir] = true
		k, err := readKustomization(dir)
		if err != nil {
			return err
		}

		if len(k.HelmCharts) > 0 {
			chartHome := "charts" // kustomize's default when helmGlobals is absent
			if k.HelmGlobals != nil && k.HelmGlobals.ChartHome != "" {
				chartHome = k.HelmGlobals.ChartHome
			}
			addRoot(resolve(dir, chartHome))
		}
		for _, hc := range k.HelmCharts {
			files := hc.AdditionalValuesFiles
			if hc.ValuesFile != "" {
				files = append([]string{hc.ValuesFile}, files...)
			}
			for _, vf := range files {
				if isRemote(vf) {
					continue
				}
				addRoot(resolve(dir, vf))
			}
		}

		//nolint:staticcheck // Bases is deprecated but kustomize still honors it; treat it like resources.
		for _, ref := range slices.Concat(k.Resources, k.Components, k.Bases) {
			if isRemote(ref) {
				continue
			}
			p := resolve(dir, ref)
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				addRoot(p)
				// Inside-root subdirectories are not dependencies themselves
				// but their kustomizations can reference paths that are.
				_ = walk(p)
			} else {
				// Files, and paths that are missing or transiently broken —
				// outside ones are still worth watching.
				addRoot(p)
			}
		}
		return nil
	}
	if err := walk(appDir); err != nil {
		return nil, err
	}
	return roots, nil
}

func readKustomization(dir string) (*types.Kustomization, error) {
	for _, name := range konfig.RecognizedKustomizationFileNames() {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var k types.Kustomization
		// Non-strict on purpose: this parser only extracts path references;
		// validating the kustomization is the renderer's job.
		if err := yaml.Unmarshal(data, &k); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", filepath.Join(dir, name), err)
		}
		return &k, nil
	}
	return nil, fmt.Errorf("no kustomization file in %s", dir)
}

// isRemote filters references kustomize would fetch rather than read from
// disk (URLs and github-style git specs); they cannot be watched.
func isRemote(ref string) bool {
	return strings.Contains(ref, "://") ||
		strings.HasPrefix(ref, "github.com/") ||
		strings.HasPrefix(ref, "git@")
}

func resolve(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}
