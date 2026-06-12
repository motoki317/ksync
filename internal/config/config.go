// Package config loads and validates ksync.yaml — the single file declaring
// which directories are apps, the one kubectl context ksync may touch, and the
// dependency edges between apps. Model rationale:
// docs/ADR/20260612-app-model-and-config.md.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/yaml"
)

// Config is the validated content of a ksync.yaml.
type Config struct {
	// Context is the kubectl context ksync targets. It is the only context
	// ksync will ever use — there is deliberately no fallback to the ambient
	// current-context, so a config can never accidentally point at production.
	Context string `json:"context"`
	Apps    []App  `json:"apps"`
}

// App is one kustomization directory managed by ksync.
type App struct {
	// Name identifies the app in the CLI and becomes the value of the ksync
	// tracking label, which is why it must be a valid Kubernetes label value.
	// Defaults to the basename of Path.
	Name string `json:"name,omitempty"`
	// Path is the directory containing the kustomization file. Relative paths
	// are resolved against the directory of the config file.
	Path string `json:"path"`
	// Namespace is the default namespace for rendered resources that set
	// none — the equivalent of an ArgoCD Application's destination.namespace.
	// When set, ksync also creates the namespace on sync if it is missing
	// (it never modifies an existing one).
	Namespace string `json:"namespace,omitempty"`
	// Needs lists apps that must be synced before this one.
	Needs []string `json:"needs,omitempty"`
	// Build lists images built from local sources for this app. When a
	// build's watched sources change, ksync rebuilds the image and injects
	// the new tag into the rendered manifests before syncing.
	Build []Build `json:"build,omitempty"`
}

// Build declares one locally built image. Design rationale:
// docs/ADR/20260612-build-integration.md.
type Build struct {
	// Image is the image name exactly as the rendered manifests reference it
	// (no tag or digest) — it selects which image fields the built tag is
	// injected into, with kustomize `images:` matching semantics.
	Image string `json:"image"`
	// Context is the docker build context directory. Relative paths are
	// resolved against the config file's directory.
	Context string `json:"context"`
	// Dockerfile is resolved against Context (docker-compose convention).
	// Defaults to "Dockerfile" in the context; unused with Command.
	Dockerfile string `json:"dockerfile,omitempty"`
	// Watch narrows which paths (resolved against Context) dirty this build.
	// Default: the whole context, minus .dockerignore exclusions — needed for
	// monorepos where one shared context feeds many images.
	Watch []string `json:"watch,omitempty"`
	// Command replaces `docker build` for flows it cannot express (bake,
	// host-side compilation, …). It runs via `sh -c` in the context directory
	// and must leave the image tagged $KSYNC_IMAGE in the local docker daemon.
	Command string `json:"command,omitempty"`
}

// Load reads, parses, and validates the config file at path, additionally
// checking that every app directory contains a kustomization file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Absolute base dir, so every resolved path is absolute no matter how -f
	// was given: subprocesses with their own working directory (docker build)
	// and watcher path matching must not depend on ksync's cwd.
	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data, baseDir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var errs []error
	for i, app := range cfg.Apps {
		if !hasKustomizationFile(app.Path) {
			errs = append(errs, fmt.Errorf("apps[%d] (%s): no kustomization file in %s", i, app.Name, app.Path))
		}
		for j, b := range app.Build {
			where := fmt.Sprintf("apps[%d] (%s) build[%d]", i, app.Name, j)
			if st, err := os.Stat(b.Context); err != nil || !st.IsDir() {
				errs = append(errs, fmt.Errorf("%s: build context %s is not a directory", where, b.Context))
				continue
			}
			if b.Command == "" {
				if st, err := os.Stat(b.Dockerfile); err != nil || st.IsDir() {
					errs = append(errs, fmt.Errorf("%s: no Dockerfile at %s (set dockerfile: or command:)", where, b.Dockerfile))
				}
			}
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", path, errors.Join(errs...))
	}
	return cfg, nil
}

// Parse parses and validates a config document. Relative app paths are
// resolved against baseDir (the config file's directory); defaults (app name
// from the path basename) are applied. All validation errors are reported
// together rather than one at a time.
func Parse(data []byte, baseDir string) (*Config, error) {
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return nil, err
	}

	var errs []error
	if cfg.Context == "" {
		errs = append(errs, errors.New("context is required"))
	}
	if len(cfg.Apps) == 0 {
		errs = append(errs, errors.New("at least one app is required"))
	}

	names := make(map[string]bool, len(cfg.Apps))
	paths := make(map[string]bool, len(cfg.Apps))
	for i := range cfg.Apps {
		app := &cfg.Apps[i]
		if app.Path == "" {
			errs = append(errs, fmt.Errorf("apps[%d]: path is required", i))
			continue
		}
		if !filepath.IsAbs(app.Path) {
			app.Path = filepath.Join(baseDir, app.Path)
		}
		app.Path = filepath.Clean(app.Path)
		if app.Name == "" {
			app.Name = filepath.Base(app.Path)
		}
		if msgs := validation.IsValidLabelValue(app.Name); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("apps[%d]: name %q must be a valid Kubernetes label value (it becomes the ksync tracking label): %s",
				i, app.Name, strings.Join(msgs, "; ")))
		}
		if app.Namespace != "" {
			if msgs := validation.IsDNS1123Label(app.Namespace); len(msgs) > 0 {
				errs = append(errs, fmt.Errorf("apps[%d] (%s): namespace %q is not a valid namespace name: %s",
					i, app.Name, app.Namespace, strings.Join(msgs, "; ")))
			}
		}
		if names[app.Name] {
			errs = append(errs, fmt.Errorf("apps[%d]: duplicate name %q", i, app.Name))
		}
		names[app.Name] = true
		if paths[app.Path] {
			errs = append(errs, fmt.Errorf("apps[%d] (%s): duplicate path %q", i, app.Name, app.Path))
		}
		paths[app.Path] = true
	}

	// One build definition per image, across all apps: two recipes for the
	// same name would race over which tag the manifests get.
	imageOwner := make(map[string]string)
	for i := range cfg.Apps {
		app := &cfg.Apps[i]
		for j := range app.Build {
			b := &app.Build[j]
			where := fmt.Sprintf("apps[%d] (%s) build[%d]", i, app.Name, j)
			switch {
			case b.Image == "":
				errs = append(errs, fmt.Errorf("%s: image is required", where))
			case !validImageName(b.Image):
				errs = append(errs, fmt.Errorf("%s: image %q must be the bare image name as the manifests reference it, without a tag or digest (ksync manages the tag)", where, b.Image))
			case imageOwner[b.Image] != "":
				errs = append(errs, fmt.Errorf("%s: image %q already has a build definition under app %q", where, b.Image, imageOwner[b.Image]))
			default:
				imageOwner[b.Image] = app.Name
			}
			if b.Context == "" {
				errs = append(errs, fmt.Errorf("%s: context is required", where))
				continue
			}
			if !filepath.IsAbs(b.Context) {
				b.Context = filepath.Join(baseDir, b.Context)
			}
			b.Context = filepath.Clean(b.Context)
			if b.Command != "" && b.Dockerfile != "" {
				errs = append(errs, fmt.Errorf("%s: command and dockerfile are mutually exclusive (a command build must produce $KSYNC_IMAGE itself)", where))
			}
			if b.Command == "" && b.Dockerfile == "" {
				b.Dockerfile = "Dockerfile"
			}
			if b.Dockerfile != "" {
				b.Dockerfile = resolveAgainst(b.Context, b.Dockerfile)
			}
			for k, w := range b.Watch {
				b.Watch[k] = resolveAgainst(b.Context, w)
			}
		}
	}

	for i := range cfg.Apps {
		app := &cfg.Apps[i]
		seen := make(map[string]bool, len(app.Needs))
		for _, dep := range app.Needs {
			if !names[dep] {
				errs = append(errs, fmt.Errorf("apps[%d] (%s): needs unknown app %q", i, app.Name, dep))
			}
			if seen[dep] {
				errs = append(errs, fmt.Errorf("apps[%d] (%s): duplicate needs entry %q", i, app.Name, dep))
			}
			seen[dep] = true
		}
	}
	if cycle := findCycle(cfg.Apps); cycle != "" {
		errs = append(errs, fmt.Errorf("dependency cycle: %s", cycle))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &cfg, nil
}

// Select returns the apps with the given names in request order, or all apps
// in declaration order when names is empty.
func (c *Config) Select(names []string) ([]App, error) {
	if len(names) == 0 {
		return c.Apps, nil
	}
	byName := make(map[string]App, len(c.Apps))
	for _, app := range c.Apps {
		byName[app.Name] = app
	}
	apps := make([]App, 0, len(names))
	for _, name := range names {
		app, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown app %q (not declared in the config)", name)
		}
		apps = append(apps, app)
	}
	return apps, nil
}

// SortByNeeds returns the apps ordered so that every app comes after the
// apps it needs, breaking ties by declaration order (the earliest-declared
// runnable app goes next). The input must already be a validated DAG.
func SortByNeeds(apps []App) []App {
	indegree := make(map[string]int, len(apps))
	dependents := make(map[string][]string, len(apps))
	for _, a := range apps {
		indegree[a.Name] = 0
	}
	for _, a := range apps {
		for _, dep := range a.Needs {
			if _, ok := indegree[dep]; ok {
				indegree[a.Name]++
				dependents[dep] = append(dependents[dep], a.Name)
			}
		}
	}
	sorted := make([]App, 0, len(apps))
	placed := make(map[string]bool, len(apps))
	for len(sorted) < len(apps) {
		progressed := false
		for _, a := range apps {
			if placed[a.Name] || indegree[a.Name] != 0 {
				continue
			}
			placed[a.Name] = true
			for _, d := range dependents[a.Name] {
				indegree[d]--
			}
			sorted = append(sorted, a)
			progressed = true
			break // rescan from the start so declaration order wins ties
		}
		if !progressed {
			return apps // cycle; unreachable after validation
		}
	}
	return sorted
}

// findCycle returns a cycle in the needs graph rendered as "a -> b -> a", or
// "" if the graph is a DAG. Apps and their needs are visited in declaration
// order so the reported cycle is deterministic.
func findCycle(apps []App) string {
	byName := make(map[string]*App, len(apps))
	for i := range apps {
		if _, ok := byName[apps[i].Name]; !ok {
			byName[apps[i].Name] = &apps[i]
		}
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(apps))
	var stack []string

	var visit func(name string) string
	visit = func(name string) string {
		state[name] = visiting
		stack = append(stack, name)
		for _, dep := range byName[name].Needs {
			if _, ok := byName[dep]; !ok {
				continue // unknown reference; reported separately
			}
			switch state[dep] {
			case visiting:
				for j, n := range stack {
					if n == dep {
						return strings.Join(append(stack[j:], dep), " -> ")
					}
				}
			case unvisited:
				if cycle := visit(dep); cycle != "" {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = done
		return ""
	}

	for i := range apps {
		if state[apps[i].Name] == unvisited {
			if cycle := visit(apps[i].Name); cycle != "" {
				return cycle
			}
		}
	}
	return ""
}

// validImageName accepts image references that carry no tag and no digest: a
// colon in the last path segment is a tag separator (earlier colons belong to
// a registry port), and "@" anywhere introduces a digest.
func validImageName(ref string) bool {
	if strings.ContainsAny(ref, "@ \t") {
		return false
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	return last != "" && !strings.Contains(last, ":")
}

func resolveAgainst(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

func hasKustomizationFile(dir string) bool {
	for _, name := range konfig.RecognizedKustomizationFileNames() {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}
