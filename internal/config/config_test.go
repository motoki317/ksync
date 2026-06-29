package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_MinimalConfig(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.AllowedContexts) != 1 || cfg.AllowedContexts[0] != "docker-desktop" {
		t.Errorf("AllowedContexts = %v, want [docker-desktop]", cfg.AllowedContexts)
	}
	if len(cfg.Apps) != 1 {
		t.Fatalf("len(Apps) = %d, want 1", len(cfg.Apps))
	}
	app := cfg.Apps[0]
	if app.Name != "api-b" {
		t.Errorf("Name = %q, want api-b (defaulted from dir basename)", app.Name)
	}
	if want := filepath.Join("/cfg", "apps", "api-b"); app.Path != want {
		t.Errorf("Path = %q, want %q (resolved against config dir)", app.Path, want)
	}
}

func TestSelectContext(t *testing.T) {
	cases := []struct {
		name     string
		allowed  []string
		override string
		want     string
		wantErr  string
	}{
		// No override: a lone concrete entry is the target; ksync never reads the
		// host current-context, so nothing else is consulted.
		{name: "single literal auto-selected", allowed: []string{"docker-desktop"}, want: "docker-desktop"},
		// No override but ambiguous: fail closed rather than guess a cluster.
		{name: "multiple contexts fail closed", allowed: []string{"docker-desktop", "k3d-dev"}, wantErr: "allowedContexts lists 2 contexts"},
		{name: "single glob has no concrete target", allowed: []string{"k3s-*"}, wantErr: "is a glob"},
		// Override disambiguates, but must still match an entry.
		{name: "override picks among many", allowed: []string{"docker-desktop", "k3d-dev"}, override: "k3d-dev", want: "k3d-dev"},
		{name: "override matches glob", allowed: []string{"docker-desktop", "k3s-*"}, override: "k3s-feature-b", want: "k3s-feature-b"},
		{name: "override anchored, no partial match", allowed: []string{"k3s-*"}, override: "prod-k3s-x", wantErr: `--context "prod-k3s-x" is not in allowedContexts`},
		{name: "override not allowed", allowed: []string{"docker-desktop", "k3d-dev"}, override: "prod-cluster", wantErr: `--context "prod-cluster" is not in allowedContexts`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{AllowedContexts: c.allowed}
			got, err := cfg.SelectContext(c.override)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectContext: %v", err)
			}
			if got != c.want {
				t.Errorf("SelectContext = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParse_ImageLoadField(t *testing.T) {
	yml := `
allowedContexts: [k3d-dev]
imageLoad:
  command: k3d image import --cluster dev $KSYNC_IMAGES
apps:
  - path: apps/api-b
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := "k3d image import --cluster dev $KSYNC_IMAGES"; cfg.ImageLoad.Command != want {
		t.Errorf("ImageLoad.Command = %q, want %q", cfg.ImageLoad.Command, want)
	}
}

func TestParse_ExplicitNameAndNeeds(t *testing.T) {
	yml := `
allowedContexts: [k3d-local]
apps:
  - name: db
    path: apps/postgres
  - name: api-b
    path: apps/api-b
    needs: [db]
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Apps[0].Name != "db" {
		t.Errorf("Apps[0].Name = %q, want db (explicit name wins over basename)", cfg.Apps[0].Name)
	}
	if len(cfg.Apps[1].Needs) != 1 || cfg.Apps[1].Needs[0] != "db" {
		t.Errorf("Apps[1].Needs = %v, want [db]", cfg.Apps[1].Needs)
	}
}

func TestParse_NamespaceField(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    namespace: team-a
  - path: apps/shop
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Apps[0].Namespace != "team-a" {
		t.Errorf("Apps[0].Namespace = %q, want team-a", cfg.Apps[0].Namespace)
	}
	if cfg.Apps[1].Namespace != "" {
		t.Errorf("Apps[1].Namespace = %q, want empty (no default)", cfg.Apps[1].Namespace)
	}
}

func TestParse_BuildEntryWithEveryField(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: example.com/team-a/api-b
        context: ../src/api-b
        dockerfile: build/Dockerfile.dev
        watch: [src, Cargo.toml]
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b := cfg.Apps[0].Build[0]
	if b.Image != "example.com/team-a/api-b" {
		t.Errorf("Image = %q", b.Image)
	}
	if want := filepath.Clean("/src/api-b"); b.Context != want {
		t.Errorf("Context = %q, want %q (resolved against config dir)", b.Context, want)
	}
	if want := filepath.Join("/src/api-b", "build", "Dockerfile.dev"); b.Dockerfile != want {
		t.Errorf("Dockerfile = %q, want %q (resolved against context)", b.Dockerfile, want)
	}
	if len(b.Watch) != 2 || b.Watch[0] != filepath.Join("/src/api-b", "src") || b.Watch[1] != filepath.Join("/src/api-b", "Cargo.toml") {
		t.Errorf("Watch = %v, want paths resolved against context", b.Watch)
	}
	if b.Name != "api-b" {
		t.Errorf("Name = %q, want %q (defaulted to the image's last path segment)", b.Name, "api-b")
	}
}

// An explicit build name overrides the image-derived default; it is what the
// watch prompt and progress rows label the image by.
func TestParse_BuildNameExplicit(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: ghcr.io/org/api-b
        name: backend
        context: ../src
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Apps[0].Build[0].Name; got != "backend" {
		t.Errorf("Name = %q, want the explicit %q", got, "backend")
	}
}

// Two builds in one app may not share a name (defaulted or explicit), or the
// prompt could not tell their rows apart.
func TestParse_BuildNameDuplicateInApp(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: ghcr.io/a/web
        context: ../src
      - image: ghcr.io/b/web
        context: ../src
`
	_, err := Parse([]byte(yml), "/cfg")
	if err == nil {
		t.Fatal("Parse: want an error for the duplicate build name, got nil")
	}
	if !strings.Contains(err.Error(), `build name "web" is already used`) {
		t.Errorf("error = %v, want it to flag the duplicate build name", err)
	}
}

func TestParse_BuildDefaultsDockerfile(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src/api-b
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := filepath.Join("/src/api-b", "Dockerfile"); cfg.Apps[0].Build[0].Dockerfile != want {
		t.Errorf("Dockerfile = %q, want %q (defaulted inside the context)", cfg.Apps[0].Build[0].Dockerfile, want)
	}
}

func TestParse_BuildCommandLeavesDockerfileEmpty(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src
        command: just build-api-b
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Apps[0].Build[0].Dockerfile; got != "" {
		t.Errorf("Dockerfile = %q, want empty (a command build has no Dockerfile to default)", got)
	}
}

func TestParse_BuildGroup(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
buildGroups:
  - name: go-components
    command: docker buildx bake $KSYNC_IMAGES
apps:
  - path: apps/ns
    build:
      - image: ghcr.io/team-a/controller
        context: ..
        watch: [cmd, pkg]
        group: go-components
      - image: ghcr.io/team-a/gateway
        context: ..
        watch: [cmd, pkg]
        group: go-components
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.BuildGroups) != 1 || cfg.BuildGroups[0].Name != "go-components" {
		t.Fatalf("BuildGroups = %+v", cfg.BuildGroups)
	}
	for i, b := range cfg.Apps[0].Build {
		if b.Group != "go-components" {
			t.Errorf("build[%d].Group = %q, want go-components", i, b.Group)
		}
		if b.Dockerfile != "" {
			t.Errorf("build[%d].Dockerfile = %q, want empty (the group builds it)", i, b.Dockerfile)
		}
		if want := filepath.Clean("/"); b.Context != want {
			t.Errorf("build[%d].Context = %q, want %q", i, b.Context, want)
		}
	}
}

func TestApp_BuildBatches(t *testing.T) {
	app := App{Build: []Build{
		{Image: "dashboard"},               // 0 ungrouped
		{Image: "controller", Group: "go"}, // 1
		{Image: "sablier"},                 // 2 ungrouped
		{Image: "gateway", Group: "go"},    // 3
		{Image: "migrate", Group: "go"},    // 4
	}}

	// All entries dirty: each ungrouped is its own batch; the group coalesces at
	// its first member's position.
	got := app.BuildBatches([]int{0, 1, 2, 3, 4})
	want := [][]int{{0}, {1, 3, 4}, {2}}
	if !equalBatches(got, want) {
		t.Errorf("BuildBatches(all) = %v, want %v", got, want)
	}

	// Only some group members dirty: the batch carries just those.
	got = app.BuildBatches([]int{3, 4})
	want = [][]int{{3, 4}}
	if !equalBatches(got, want) {
		t.Errorf("BuildBatches(partial group) = %v, want %v", got, want)
	}

	// A single ungrouped entry.
	got = app.BuildBatches([]int{2})
	want = [][]int{{2}}
	if !equalBatches(got, want) {
		t.Errorf("BuildBatches(single) = %v, want %v", got, want)
	}
}

func equalBatches(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func TestParse_BuildImageWithRegistryPortIsValid(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: localhost:5000/team-a/api-b
        context: ../src/api-b
`
	if _, err := Parse([]byte(yml), "/cfg"); err != nil {
		t.Fatalf("Parse rejected a registry-port image name: %v", err)
	}
}

func TestParse_BuildValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		yml     string
		wantErr []string
	}{
		{
			name: "build without image",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - context: ../src/api-b
`,
			wantErr: []string{"apps[0] (api-b) build[0]", "image is required"},
		},
		{
			name: "image with tag",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b:dev
        context: ../src/api-b
`,
			wantErr: []string{"build[0]", `image "api-b:dev"`, "tag"},
		},
		{
			name: "image with digest",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b@sha256:abc
        context: ../src/api-b
`,
			wantErr: []string{"build[0]", `image "api-b@sha256:abc"`},
		},
		{
			name: "build without context",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b
`,
			wantErr: []string{"apps[0] (api-b) build[0]", "context is required"},
		},
		{
			name: "command and dockerfile together",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src
        dockerfile: Dockerfile.dev
        command: just build
`,
			wantErr: []string{"build[0]", "command", "dockerfile"},
		},
		{
			name: "duplicate image within an app",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src/one
      - image: api-b
        context: ../src/two
`,
			wantErr: []string{"build[1]", `image "api-b"`, "already"},
		},
		{
			name: "duplicate image across apps",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: shared
        context: ../src/one
  - path: apps/shop
    build:
      - image: shared
        context: ../src/two
`,
			wantErr: []string{"apps[1] (shop) build[0]", `image "shared"`, `"api-b"`},
		},
		{
			name: "grouped build references unknown group",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src
        group: nope
`,
			wantErr: []string{"build[0]", "unknown build group", `"nope"`},
		},
		{
			name: "grouped build also sets command",
			yml: `allowedContexts: [docker-desktop]
buildGroups:
  - name: g
    command: bake
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src
        group: g
        command: just build
`,
			wantErr: []string{"build[0]", "neither command nor dockerfile", `"g"`},
		},
		{
			name: "build group without command",
			yml: `allowedContexts: [docker-desktop]
buildGroups:
  - name: g
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src
        group: g
`,
			wantErr: []string{"buildGroups[0] (g)", "command is required"},
		},
		{
			name: "duplicate build group name",
			yml: `allowedContexts: [docker-desktop]
buildGroups:
  - name: g
    command: bake
  - name: g
    command: bake2
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src
        group: g
`,
			wantErr: []string{"buildGroups[1]", "duplicate group name", `"g"`},
		},
		{
			name: "build group with no members",
			yml: `allowedContexts: [docker-desktop]
buildGroups:
  - name: g
    command: bake
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src
`,
			wantErr: []string{"buildGroups[0] (g)", "no build entry joins"},
		},
		{
			name: "build group mixes contexts",
			yml: `allowedContexts: [docker-desktop]
buildGroups:
  - name: g
    command: bake
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: ../src/one
        group: g
      - image: api-c
        context: ../src/two
        group: g
`,
			wantErr: []string{"build[1]", "mixes contexts", `"g"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yml), "/cfg")
			if err == nil {
				t.Fatal("Parse succeeded, want validation error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestParse_AbsolutePathKeptAsIs(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: /elsewhere/apps/shop
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := filepath.Clean("/elsewhere/apps/shop"); cfg.Apps[0].Path != want {
		t.Errorf("Path = %q, want %q (absolute paths are not re-resolved)", cfg.Apps[0].Path, want)
	}
}

func TestParse_RejectsUnknownFields(t *testing.T) {
	yml := `
allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    need: [db]
`
	_, err := Parse([]byte(yml), "/cfg")
	if err == nil {
		t.Fatal("Parse accepted an unknown field (typo \"need\"); strict parsing must reject it")
	}
}

func TestParse_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		yml     string
		wantErr []string // substrings that must all appear in the error
	}{
		{
			name:    "missing allowedContexts",
			yml:     "apps:\n  - path: apps/api-b\n",
			wantErr: []string{"allowedContexts must list at least one"},
		},
		{
			name:    "empty allowedContexts entry",
			yml:     "allowedContexts: [\"\"]\napps:\n  - path: apps/api-b\n",
			wantErr: []string{"allowedContexts[0]", "empty context name"},
		},
		{
			name:    "malformed glob in allowedContexts",
			yml:     "allowedContexts: [\"k3s-[\"]\napps:\n  - path: apps/api-b\n",
			wantErr: []string{"allowedContexts[0]", "invalid glob pattern"},
		},
		{
			name:    "no apps",
			yml:     "allowedContexts: [docker-desktop]\n",
			wantErr: []string{"at least one app"},
		},
		{
			name:    "app without path",
			yml:     "allowedContexts: [docker-desktop]\napps:\n  - name: api-b\n",
			wantErr: []string{"apps[0]", "path is required"},
		},
		{
			name: "invalid namespace",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    namespace: Not_A_Namespace
`,
			wantErr: []string{"apps[0]", `namespace "Not_A_Namespace"`},
		},
		{
			name: "duplicate explicit names",
			yml: `allowedContexts: [docker-desktop]
apps:
  - name: api-b
    path: apps/one
  - name: api-b
    path: apps/two
`,
			wantErr: []string{"apps[1]", `duplicate name "api-b"`},
		},
		{
			name: "duplicate defaulted names",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: team-a/api-b
  - path: team-b/api-b
`,
			wantErr: []string{"apps[1]", `duplicate name "api-b"`},
		},
		{
			name: "duplicate paths after cleaning",
			yml: `allowedContexts: [docker-desktop]
apps:
  - name: one
    path: apps/api-b
  - name: two
    path: ./apps/api-b
`,
			wantErr: []string{"apps[1]", "duplicate path"},
		},
		{
			name: "name not a valid label value",
			yml: `allowedContexts: [docker-desktop]
apps:
  - name: -api
    path: apps/api-b
`,
			wantErr: []string{"apps[0]", `name "-api"`, "label value"},
		},
		{
			name: "needs references unknown app",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    needs: [ghost]
`,
			wantErr: []string{"apps[0] (api-b)", `unknown app "ghost"`},
		},
		{
			name: "needs lists the same app twice",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/db
  - path: apps/api-b
    needs: [db, db]
`,
			wantErr: []string{"apps[1] (api-b)", `duplicate needs entry "db"`},
		},
		{
			name: "self dependency",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    needs: [api-b]
`,
			wantErr: []string{"dependency cycle", "api-b -> api-b"},
		},
		{
			name: "dependency cycle",
			yml: `allowedContexts: [docker-desktop]
apps:
  - path: apps/a
    needs: [b]
  - path: apps/b
    needs: [c]
  - path: apps/c
    needs: [a]
`,
			wantErr: []string{"dependency cycle", "a -> b -> c -> a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yml), "/cfg")
			if err == nil {
				t.Fatal("Parse succeeded, want validation error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestParse_ReportsAllErrorsTogether(t *testing.T) {
	yml := `
apps:
  - name: api-b
  - path: apps/api-b
    needs: [ghost]
`
	_, err := Parse([]byte(yml), "/cfg")
	if err == nil {
		t.Fatal("Parse succeeded, want validation errors")
	}
	for _, want := range []string{"allowedContexts must list at least one", "path is required", `unknown app "ghost"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q; all validation errors must be reported at once", err, want)
		}
	}
}

func TestLoad_MissingFileGivesActionableMessage(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("Load of a missing file succeeded, want an error")
	}
	for _, want := range []string{"no config file", "-f"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestMultiErr_FormatsManyAsIndentedList(t *testing.T) {
	one := multiErr{errFor("only one")}
	if got := one.Error(); got != "only one" {
		t.Errorf("single error = %q, want it unwrapped without a list header", got)
	}
	many := multiErr{errFor("first"), errFor("second")}
	got := many.Error()
	for _, want := range []string{"2 problems:", "\n  - first", "\n  - second"} {
		if !strings.Contains(got, want) {
			t.Errorf("multi error %q missing %q", got, want)
		}
	}
}

func errFor(msg string) error { return &simpleErr{msg} }

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

func TestLoad_ResolvesPathsRelativeToConfigFile(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "apps", "api-b")
	mustMkdirAll(t, appDir)
	mustWriteFile(t, filepath.Join(appDir, "kustomization.yaml"), "resources: []\n")
	cfgPath := filepath.Join(dir, "ksync.yaml")
	mustWriteFile(t, cfgPath, "allowedContexts: [docker-desktop]\napps:\n  - path: apps/api-b\n")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Apps[0].Path != appDir {
		t.Errorf("Path = %q, want %q", cfg.Apps[0].Path, appDir)
	}
}

func TestLoad_RejectsAppDirWithoutKustomization(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "apps", "api-b"))
	cfgPath := filepath.Join(dir, "ksync.yaml")
	mustWriteFile(t, cfgPath, "allowedContexts: [docker-desktop]\napps:\n  - path: apps/api-b\n")

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load accepted an app dir with no kustomization file")
	}
	if !strings.Contains(err.Error(), "no kustomization file") {
		t.Errorf("error %q does not mention the missing kustomization file", err)
	}
}

func TestLoad_AcceptsAnyRecognizedKustomizationFileName(t *testing.T) {
	// kustomize recognizes three file names; ksync must accept all of them.
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			appDir := filepath.Join(dir, "apps", "api-b")
			mustMkdirAll(t, appDir)
			mustWriteFile(t, filepath.Join(appDir, name), "resources: []\n")
			cfgPath := filepath.Join(dir, "ksync.yaml")
			mustWriteFile(t, cfgPath, "allowedContexts: [docker-desktop]\napps:\n  - path: apps/api-b\n")

			if _, err := Load(cfgPath); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

func TestLoad_RelativeConfigPathYieldsAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "apps/api-b")
	mustWriteFile(t, filepath.Join(dir, "ksync.yaml"), "allowedContexts: [docker-desktop]\napps:\n  - path: apps/api-b\n")
	t.Chdir(dir)

	cfg, err := Load("ksync.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !filepath.IsAbs(cfg.Apps[0].Path) {
		t.Errorf("Path = %q, want absolute — subprocess working dirs and watcher matching must not depend on ksync's cwd", cfg.Apps[0].Path)
	}
}

func TestLoad_RejectsMissingBuildContextDir(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "apps/api-b")
	cfgPath := filepath.Join(dir, "ksync.yaml")
	mustWriteFile(t, cfgPath, `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: src/api-b
`)
	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "build context") {
		t.Fatalf("Load = %v, want missing build context error", err)
	}
}

func TestLoad_RejectsMissingDockerfile(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "apps/api-b")
	mustMkdirAll(t, filepath.Join(dir, "src", "api-b"))
	cfgPath := filepath.Join(dir, "ksync.yaml")
	mustWriteFile(t, cfgPath, `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: src/api-b
`)
	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "Dockerfile") {
		t.Fatalf("Load = %v, want missing Dockerfile error", err)
	}
}

func TestLoad_CommandBuildNeedsNoDockerfile(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "apps/api-b")
	mustMkdirAll(t, filepath.Join(dir, "src", "api-b"))
	cfgPath := filepath.Join(dir, "ksync.yaml")
	mustWriteFile(t, cfgPath, `allowedContexts: [docker-desktop]
apps:
  - path: apps/api-b
    build:
      - image: api-b
        context: src/api-b
        command: just build
`)
	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func writeApp(t *testing.T, dir, rel string) {
	t.Helper()
	appDir := filepath.Join(dir, rel)
	mustMkdirAll(t, appDir)
	mustWriteFile(t, filepath.Join(appDir, "kustomization.yaml"), "resources: []\n")
}

func TestSelect_EmptyNamesMeansAllApps(t *testing.T) {
	cfg := &Config{Apps: []App{{Name: "db"}, {Name: "api-b"}}}
	apps, err := cfg.Select(nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(apps) != 2 || apps[0].Name != "db" || apps[1].Name != "api-b" {
		t.Errorf("Select(nil) = %v, want all apps in declaration order", apps)
	}
}

func TestSelect_ReturnsNamedAppsInRequestOrder(t *testing.T) {
	cfg := &Config{Apps: []App{{Name: "db"}, {Name: "api-b"}, {Name: "shop"}}}
	apps, err := cfg.Select([]string{"shop", "db"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(apps) != 2 || apps[0].Name != "shop" || apps[1].Name != "db" {
		t.Errorf("Select = %v, want [shop db]", apps)
	}
}

func TestSelect_RejectsUnknownName(t *testing.T) {
	cfg := &Config{Apps: []App{{Name: "db"}}}
	_, err := cfg.Select([]string{"ghost"})
	if err == nil || !strings.Contains(err.Error(), `unknown app "ghost"`) {
		t.Errorf("Select error = %v, want unknown app error", err)
	}
}

func TestSortByNeeds_KeepsDeclarationOrderWhenUnconstrained(t *testing.T) {
	apps := []App{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := SortByNeeds(apps)
	if !sameOrder(got, "a", "b", "c") {
		t.Errorf("SortByNeeds = %v, want declaration order", names(got))
	}
}

func TestSortByNeeds_PlacesDependenciesFirst(t *testing.T) {
	apps := []App{{Name: "api-b", Needs: []string{"db"}}, {Name: "db"}}
	got := SortByNeeds(apps)
	if !sameOrder(got, "db", "api-b") {
		t.Errorf("SortByNeeds = %v, want [db api-b]", names(got))
	}
}

func TestSortByNeeds_BreaksTiesByDeclarationOrder(t *testing.T) {
	// Diamond: top is needed by both mid apps; bottom needs both mids.
	apps := []App{
		{Name: "bottom", Needs: []string{"mid-b", "mid-a"}},
		{Name: "mid-b", Needs: []string{"top"}},
		{Name: "mid-a", Needs: []string{"top"}},
		{Name: "top"},
	}
	got := SortByNeeds(apps)
	if !sameOrder(got, "top", "mid-b", "mid-a", "bottom") {
		t.Errorf("SortByNeeds = %v, want [top mid-b mid-a bottom]", names(got))
	}
}

func sameOrder(apps []App, want ...string) bool {
	if len(apps) != len(want) {
		return false
	}
	for i, a := range apps {
		if a.Name != want[i] {
			return false
		}
	}
	return true
}

func names(apps []App) []string {
	out := make([]string, len(apps))
	for i, a := range apps {
		out[i] = a.Name
	}
	return out
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
