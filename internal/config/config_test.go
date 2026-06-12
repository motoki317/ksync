package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_MinimalConfig(t *testing.T) {
	yml := `
context: docker-desktop
apps:
  - path: apps/api-b
`
	cfg, err := Parse([]byte(yml), "/cfg")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Context != "docker-desktop" {
		t.Errorf("Context = %q, want docker-desktop", cfg.Context)
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

func TestParse_ExplicitNameAndNeeds(t *testing.T) {
	yml := `
context: k3d-local
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

func TestParse_AbsolutePathKeptAsIs(t *testing.T) {
	yml := `
context: docker-desktop
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
context: docker-desktop
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
			name:    "missing context",
			yml:     "apps:\n  - path: apps/api-b\n",
			wantErr: []string{"context is required"},
		},
		{
			name:    "no apps",
			yml:     "context: docker-desktop\n",
			wantErr: []string{"at least one app"},
		},
		{
			name:    "app without path",
			yml:     "context: docker-desktop\napps:\n  - name: api-b\n",
			wantErr: []string{"apps[0]", "path is required"},
		},
		{
			name: "duplicate explicit names",
			yml: `context: docker-desktop
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
			yml: `context: docker-desktop
apps:
  - path: team-a/api-b
  - path: team-b/api-b
`,
			wantErr: []string{"apps[1]", `duplicate name "api-b"`},
		},
		{
			name: "duplicate paths after cleaning",
			yml: `context: docker-desktop
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
			yml: `context: docker-desktop
apps:
  - name: -api
    path: apps/api-b
`,
			wantErr: []string{"apps[0]", `name "-api"`, "label value"},
		},
		{
			name: "needs references unknown app",
			yml: `context: docker-desktop
apps:
  - path: apps/api-b
    needs: [ghost]
`,
			wantErr: []string{"apps[0] (api-b)", `unknown app "ghost"`},
		},
		{
			name: "needs lists the same app twice",
			yml: `context: docker-desktop
apps:
  - path: apps/db
  - path: apps/api-b
    needs: [db, db]
`,
			wantErr: []string{"apps[1] (api-b)", `duplicate needs entry "db"`},
		},
		{
			name: "self dependency",
			yml: `context: docker-desktop
apps:
  - path: apps/api-b
    needs: [api-b]
`,
			wantErr: []string{"dependency cycle", "api-b -> api-b"},
		},
		{
			name: "dependency cycle",
			yml: `context: docker-desktop
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
	for _, want := range []string{"context is required", "path is required", `unknown app "ghost"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q; all validation errors must be reported at once", err, want)
		}
	}
}

func TestLoad_ResolvesPathsRelativeToConfigFile(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "apps", "api-b")
	mustMkdirAll(t, appDir)
	mustWriteFile(t, filepath.Join(appDir, "kustomization.yaml"), "resources: []\n")
	cfgPath := filepath.Join(dir, "ksync.yaml")
	mustWriteFile(t, cfgPath, "context: docker-desktop\napps:\n  - path: apps/api-b\n")

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
	mustWriteFile(t, cfgPath, "context: docker-desktop\napps:\n  - path: apps/api-b\n")

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
			mustWriteFile(t, cfgPath, "context: docker-desktop\napps:\n  - path: apps/api-b\n")

			if _, err := Load(cfgPath); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
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
