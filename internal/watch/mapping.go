// Package watch turns file-system changes into per-app work: it derives which
// paths outside an app directory must be observed (DependencyRoots) and maps a
// changed path to the apps whose rendered output may differ (Mapping). The
// fsnotify-driven watcher itself lands with the watch loop.
package watch

import (
	"path/filepath"
	"strings"
)

// AppRoots associates an app with the directory (or file) roots whose changes
// dirty it: the app dir itself plus its DependencyRoots.
type AppRoots struct {
	App   string
	Roots []string
}

// Mapping resolves changed file paths to the apps that must re-render.
type Mapping struct {
	apps []AppRoots
}

// NewMapping builds a Mapping. Roots are cleaned; callers must pass changed
// paths in the same form (absolute vs relative) as the roots.
func NewMapping(apps []AppRoots) *Mapping {
	cleaned := make([]AppRoots, len(apps))
	for i, a := range apps {
		roots := make([]string, len(a.Roots))
		for j, r := range a.Roots {
			roots[j] = filepath.Clean(r)
		}
		cleaned[i] = AppRoots{App: a.App, Roots: roots}
	}
	return &Mapping{apps: cleaned}
}

// AffectedBy returns the apps dirtied by a change at path, in declaration
// order, each at most once.
func (m *Mapping) AffectedBy(path string) []string {
	path = filepath.Clean(path)
	var affected []string
	for _, a := range m.apps {
		for _, root := range a.Roots {
			if path == root || isBelow(root, path) {
				affected = append(affected, a.App)
				break
			}
		}
	}
	return affected
}

// AllRoots returns every distinct root in declaration order — the set of
// paths a watcher must observe.
func (m *Mapping) AllRoots() []string {
	seen := map[string]bool{}
	var roots []string
	for _, a := range m.apps {
		for _, root := range a.Roots {
			if !seen[root] {
				seen[root] = true
				roots = append(roots, root)
			}
		}
	}
	return roots
}

// isBelow reports whether path is strictly inside root. Matching must respect
// path boundaries: /a/b is not a parent of /a/bc.
func isBelow(root, path string) bool {
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
