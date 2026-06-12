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

	"k8s.io/apimachinery/pkg/api/validate/content"
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
	// Needs lists apps that must be synced before this one.
	Needs []string `json:"needs,omitempty"`
}

// Load reads, parses, and validates the config file at path, additionally
// checking that every app directory contains a kustomization file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var errs []error
	for i, app := range cfg.Apps {
		if !hasKustomizationFile(app.Path) {
			errs = append(errs, fmt.Errorf("apps[%d] (%s): no kustomization file in %s", i, app.Name, app.Path))
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
		if msgs := content.IsLabelValue(app.Name); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("apps[%d]: name %q must be a valid Kubernetes label value (it becomes the ksync tracking label): %s",
				i, app.Name, strings.Join(msgs, "; ")))
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

// Recognized kustomization file names, kept in sync with kustomize's
// konfig.RecognizedKustomizationFileNames; hardcoded until the kustomize
// dependency lands with the renderer.
var kustomizationFileNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

func hasKustomizationFile(dir string) bool {
	for _, name := range kustomizationFileNames {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}
