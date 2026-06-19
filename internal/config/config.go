// Package config loads and validates ksync.yaml — the single file declaring
// which directories are apps, the one kubectl context ksync may touch, and the
// dependency edges between apps. Model rationale:
// docs/ADR/20260612-app-model-and-config.md.
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/yaml"
)

// Config is the validated content of a ksync.yaml.
type Config struct {
	// AllowedContexts is the allowlist of kubectl contexts ksync may target, and
	// also the source of the target: ksync never reads the kubeconfig
	// current-context (a host-global setting every other shell shares). When this
	// names exactly one concrete context, a run with no --context targets it
	// automatically; an explicit --context must match an entry here. A glob entry
	// or a second entry makes the target ambiguous, so a run without --context
	// fails closed rather than guess — there is no path by which a config acts on a
	// cluster it does not name. The allowlist is what lets one ksync.yaml serve
	// several interchangeable dev clusters (e.g. a shared-daemon Docker Desktop and
	// a separate-store local k3s), chosen per run with --context, while still
	// refusing production.
	//
	// Each entry is a shell-style glob (path.Match: `*`, `?`, `[…]`); a name with
	// no metacharacters matches exactly. A glob lets one entry cover a family of
	// clusters whose context names are not known up front — per-worktree/per-branch
	// microVMs named `k3s-feature-a`, `k3s-feature-b`, … all matched by `k3s-*`.
	// Globs only widen the allowlist, so keep them tight: `*` matches every
	// context, which defeats the safety gate.
	AllowedContexts []string `json:"allowedContexts"`
	// ImageLoad makes freshly built images visible to a cluster whose image
	// store is separate from the local docker daemon (k3d, kind, a remote
	// registry). Omit it for daemon-shared clusters (Docker Desktop).
	ImageLoad ImageLoad `json:"imageLoad,omitempty"`
	// BuildGroups batch several build entries into one bulk command (see
	// BuildGroup). Optional; a config can build every image individually.
	BuildGroups []BuildGroup `json:"buildGroups,omitempty"`
	Apps        []App        `json:"apps"`
}

// ImageLoad is the hook that makes freshly built images visible to a cluster
// with a separate image store. Command runs via `sh -c` with $KSYNC_IMAGES set
// to the newline-separated built refs ($KSYNC_IMAGE holds the first, for the
// single-image case) — the same contract as a build command — so ksync needs no
// per-cluster-type knowledge. Loads never overlap (some importers, notably `k3d
// image import`, corrupt under concurrency); instead, images that finish while a
// load runs are coalesced into the next invocation's $KSYNC_IMAGES, so a
// bulk-capable command amortizes its per-invocation cost. A command that cannot
// take many images at once should loop over $KSYNC_IMAGES itself. Examples:
//
//	command: k3d image import --cluster dev $KSYNC_IMAGES
//	command: kind load docker-image --name dev $KSYNC_IMAGES
//	command: docker save $KSYNC_IMAGES | k3s ctr -n k8s.io images import -
//	command: for i in $KSYNC_IMAGES; do docker push "$i"; done
type ImageLoad struct {
	Command string `json:"command"`
}

// BuildGroup batches several build entries into a single bulk command — one
// `docker buildx bake <targets>`, one host compile producing many images — so
// shared work (a common base image, a single compiler pass, one cluster import)
// happens once instead of once per image. A build entry joins a group by
// setting its `group` field to the group's name. Design rationale:
// docs/ADR/20260614-bulk-build-groups.md.
type BuildGroup struct {
	// Name is what a build entry's `group` field references.
	Name string `json:"name"`
	// Command runs via `sh -c` and must leave every requested image tagged
	// <image>:ksync-build in the local docker daemon. It receives $KSYNC_IMAGES:
	// the newline-separated temp refs to produce — only the dirty subset of the
	// group, which may be a single image. This mirrors the single-build
	// `command` contract ($KSYNC_IMAGE), scaled to many images. It runs in the
	// shared context directory of the group's members.
	Command string `json:"command"`
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
	// Name is the short label for this build in progress output and in the watch
	// confirmation prompt (where it names one selectable image). It defaults to
	// the image's last path segment (ghcr.io/org/api-b → api-b) and must be
	// unique within an app, so two of its builds are never ambiguous in the
	// picker; cross-app duplicates are fine (the prompt qualifies each by app).
	Name string `json:"name,omitempty"`
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
	// WatchIgnore lists patterns (.dockerignore syntax, relative to Context)
	// excluded from change detection but NOT from the build context. It is for
	// build outputs staged inside the context — a host-compiled binary the
	// Dockerfile COPYs — which docker must still receive yet must not re-trigger
	// the build that wrote them. .dockerignore normally breaks that self-trigger
	// loop, but it cannot here: dockerignoring the path would drop it from the
	// COPY. Watching-only ignores close that gap.
	WatchIgnore []string `json:"watchIgnore,omitempty"`
	// Command replaces `docker build` for flows it cannot express (bake,
	// host-side compilation, …). It runs via `sh -c` in the context directory
	// and must leave the image tagged $KSYNC_IMAGE in the local docker daemon.
	Command string `json:"command,omitempty"`
	// Group, when set, makes this image part of a BuildGroup of that name: its
	// build is delegated to the group's bulk command instead of a per-image
	// docker build or command. A grouped entry sets neither command nor
	// dockerfile (the group builds it); context and watch still scope what
	// dirties it.
	Group string `json:"group,omitempty"`
}

// BuildBatches partitions the given build-entry indices of a into the batches
// the loop builds as a unit: each ungrouped entry is its own batch (one docker
// build / command), and the entries of one group coalesce into a single batch
// (one bulk command). Batch order follows the first appearance of each entry in
// indices, so the build order stays deterministic. indices must be valid
// indices into a.Build.
func (a *App) BuildBatches(indices []int) [][]int {
	var batches [][]int
	groupAt := make(map[string]int) // group name -> its batch index
	for _, j := range indices {
		g := a.Build[j].Group
		if g == "" {
			batches = append(batches, []int{j})
			continue
		}
		if at, ok := groupAt[g]; ok {
			batches[at] = append(batches[at], j)
			continue
		}
		groupAt[g] = len(batches)
		batches = append(batches, []int{j})
	}
	return batches
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
			// Grouped and command builds produce the image themselves; only a
			// plain docker build needs a Dockerfile on disk.
			if b.Command == "" && b.Group == "" {
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
	if len(cfg.AllowedContexts) == 0 {
		errs = append(errs, errors.New("allowedContexts must list at least one kubectl context"))
	}
	for i, c := range cfg.AllowedContexts {
		if strings.TrimSpace(c) == "" {
			errs = append(errs, fmt.Errorf("allowedContexts[%d]: empty context name", i))
			continue
		}
		if _, err := path.Match(c, ""); err != nil {
			errs = append(errs, fmt.Errorf("allowedContexts[%d] (%q): invalid glob pattern: %v", i, c, err))
		}
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

	// Build groups must be declared before a build entry can join one. Track
	// each group's shared context (all its members must build from the same
	// directory, where its command runs) and whether any entry referenced it.
	knownGroups := make(map[string]bool, len(cfg.BuildGroups))
	groupContext := make(map[string]string, len(cfg.BuildGroups))
	groupUsed := make(map[string]bool, len(cfg.BuildGroups))
	for i := range cfg.BuildGroups {
		g := &cfg.BuildGroups[i]
		switch {
		case g.Name == "":
			errs = append(errs, fmt.Errorf("buildGroups[%d]: name is required", i))
		case knownGroups[g.Name]:
			errs = append(errs, fmt.Errorf("buildGroups[%d]: duplicate group name %q", i, g.Name))
		default:
			knownGroups[g.Name] = true
		}
		if g.Command == "" {
			errs = append(errs, fmt.Errorf("buildGroups[%d] (%s): command is required (it must produce every $KSYNC_IMAGES ref)", i, g.Name))
		}
	}

	// One build definition per image, across all apps: two recipes for the
	// same name would race over which tag the manifests get.
	imageOwner := make(map[string]string)
	for i := range cfg.Apps {
		app := &cfg.Apps[i]
		seenNames := make(map[string]bool, len(app.Build))
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
			// Default the build name to the image's last path segment and keep it
			// unique within the app, so the watch prompt can label each selectable
			// image unambiguously.
			if b.Name == "" && b.Image != "" {
				b.Name = imageBaseName(b.Image)
			}
			if b.Name != "" {
				if seenNames[b.Name] {
					errs = append(errs, fmt.Errorf("%s: build name %q is already used in this app (set a distinct name:)", where, b.Name))
				}
				seenNames[b.Name] = true
			}
			if b.Context == "" {
				errs = append(errs, fmt.Errorf("%s: context is required", where))
				continue
			}
			if !filepath.IsAbs(b.Context) {
				b.Context = filepath.Join(baseDir, b.Context)
			}
			b.Context = filepath.Clean(b.Context)
			if b.Group != "" {
				// A grouped entry is built by the group's bulk command, not by a
				// per-image docker build or command of its own.
				if b.Command != "" || b.Dockerfile != "" {
					errs = append(errs, fmt.Errorf("%s: a grouped build sets neither command nor dockerfile (group %q builds it)", where, b.Group))
				}
				if !knownGroups[b.Group] {
					errs = append(errs, fmt.Errorf("%s: unknown build group %q (declare it under buildGroups)", where, b.Group))
				} else {
					groupUsed[b.Group] = true
					if prev, ok := groupContext[b.Group]; ok && prev != b.Context {
						errs = append(errs, fmt.Errorf("%s: build group %q mixes contexts %q and %q (its command runs in one directory)", where, b.Group, prev, b.Context))
					}
					groupContext[b.Group] = b.Context
				}
				for k, w := range b.Watch {
					b.Watch[k] = resolveAgainst(b.Context, w)
				}
				continue
			}
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
	for i := range cfg.BuildGroups {
		if g := cfg.BuildGroups[i]; g.Name != "" && knownGroups[g.Name] && !groupUsed[g.Name] {
			errs = append(errs, fmt.Errorf("buildGroups[%d] (%s): no build entry joins this group", i, g.Name))
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
// SelectContext resolves which kubectl context a run targets and enforces the
// allowlist. override is the explicit --context flag ("" if unset).
//
// ksync deliberately never consults the kubeconfig's current-context: it is a
// host-global setting every other shell shares, so binding ksync's target to it
// makes a run depend on invisible external state and invites a "just switch it
// for me" implementation that would yank the context out from under those shells.
// The target therefore comes only from config and the explicit override:
//
//   - --context given: it must match an AllowedContexts entry, else refused.
//   - no override, the allowlist names exactly one concrete context: use it.
//   - otherwise (≥2 entries, or a single glob): ambiguous — fail closed.
func (c *Config) SelectContext(override string) (string, error) {
	allowed := strings.Join(c.AllowedContexts, ", ")
	if override != "" {
		for _, pat := range c.AllowedContexts {
			// Patterns are validated at parse time; a malformed one here (a Config
			// built directly, bypassing Parse) just fails to match, never errors the run.
			if ok, _ := path.Match(pat, override); ok {
				return override, nil
			}
		}
		return "", fmt.Errorf("--context %q is not in allowedContexts (%s)", override, allowed)
	}
	if len(c.AllowedContexts) == 1 && !isGlob(c.AllowedContexts[0]) {
		return c.AllowedContexts[0], nil
	}
	if len(c.AllowedContexts) == 1 {
		return "", fmt.Errorf("no kubectl context selected: the sole allowedContexts entry %q is a glob, not a concrete context; pass --context", c.AllowedContexts[0])
	}
	return "", fmt.Errorf("no kubectl context selected: allowedContexts lists %d contexts (%s); pass --context to choose one", len(c.AllowedContexts), allowed)
}

// isGlob reports whether s carries a path.Match metacharacter — i.e. it is a
// pattern that may match several context names rather than naming exactly one.
func isGlob(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

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

// imageBaseName is the default build name: the image's last path segment without
// any tag (ghcr.io/org/api-b:dev → api-b). It is only called on names that passed
// validImageName (no tag/digest), so the colon strip is just defensive.
func imageBaseName(image string) string {
	if i := strings.LastIndexByte(image, '/'); i >= 0 {
		image = image[i+1:]
	}
	if i := strings.IndexByte(image, ':'); i >= 0 {
		image = image[:i]
	}
	return image
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
