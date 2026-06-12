package build

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/motoki317/ksync/internal/config"
)

func TestDevTag(t *testing.T) {
	id := "sha256:" + strings.Repeat("ab", 32)
	tag, err := devTag(id)
	if err != nil {
		t.Fatalf("devTag: %v", err)
	}
	if tag != "ksync-abababababab" {
		t.Errorf("devTag = %q, want ksync-abababababab", tag)
	}
	if _, err := devTag("sha256:short"); err == nil {
		t.Error("devTag accepted a truncated image ID")
	}
}

type call struct {
	dir  string
	env  []string
	argv []string
}

// fakeExec scripts process execution: it emulates docker's --iidfile contract
// and answers `docker image inspect` with the given image ID.
func fakeExec(calls *[]call, imageID string, failOn func(argv []string) error) ExecFunc {
	return func(_ context.Context, dir string, env []string, argv []string, stdout, _ io.Writer) error {
		*calls = append(*calls, call{dir: dir, env: env, argv: argv})
		if failOn != nil {
			if err := failOn(argv); err != nil {
				return err
			}
		}
		if i := slices.Index(argv, "--iidfile"); i >= 0 && argv[0] == "docker" && argv[1] == "build" {
			if err := os.WriteFile(argv[i+1], []byte(imageID+"\n"), 0o644); err != nil {
				return err
			}
		}
		if argv[0] == "docker" && argv[1] == "image" && argv[2] == "inspect" {
			_, _ = io.WriteString(stdout, imageID+"\n")
		}
		return nil
	}
}

func TestBuilder_DockerfileBuild(t *testing.T) {
	ctxDir := t.TempDir()
	b := config.Build{
		Image:      "example.com/team-a/api-b",
		Context:    ctxDir,
		Dockerfile: filepath.Join(ctxDir, "Dockerfile"),
	}
	imageID := "sha256:" + strings.Repeat("0123", 16)
	var calls []call
	builder := &Builder{Exec: fakeExec(&calls, imageID, nil), Output: io.Discard}

	ref, err := builder.Build(context.Background(), b)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "example.com/team-a/api-b:ksync-012301230123"; ref != want {
		t.Errorf("ref = %q, want %q", ref, want)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (build, tag)", len(calls))
	}
	bld := calls[0]
	if bld.dir != ctxDir {
		t.Errorf("build dir = %q, want the context", bld.dir)
	}
	// --provenance=false is load-bearing: the attestation embeds timestamps,
	// so identical content would otherwise get a new ID (and roll pods) on
	// every rebuild.
	for _, want := range []string{"docker", "build", "--provenance=false", "-f", b.Dockerfile, ctxDir} {
		if !slices.Contains(bld.argv, want) {
			t.Errorf("build argv %v missing %q", bld.argv, want)
		}
	}
	// Retagging must go through the temp name: the containerd image store
	// does not resolve config digests in `docker tag`.
	tmpRef := "example.com/team-a/api-b:ksync-build"
	if !slices.Contains(bld.argv, tmpRef) {
		t.Errorf("build argv %v missing temp tag %q", bld.argv, tmpRef)
	}
	if got := calls[1].argv; !slices.Equal(got, []string{"docker", "tag", tmpRef, ref}) {
		t.Errorf("tag argv = %v, want docker tag %s %s", got, tmpRef, ref)
	}
}

func TestBuilder_CommandBuild(t *testing.T) {
	ctxDir := t.TempDir()
	b := config.Build{
		Image:   "api-b",
		Context: ctxDir,
		Command: "just build-api-b",
	}
	imageID := "sha256:" + strings.Repeat("ef", 32)
	var calls []call
	builder := &Builder{Exec: fakeExec(&calls, imageID, nil), Output: io.Discard}

	ref, err := builder.Build(context.Background(), b)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "api-b:ksync-efefefefefef"; ref != want {
		t.Errorf("ref = %q, want %q", ref, want)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3 (command, inspect, tag)", len(calls))
	}
	cmd := calls[0]
	if !slices.Equal(cmd.argv, []string{"sh", "-c", "just build-api-b"}) {
		t.Errorf("command argv = %v", cmd.argv)
	}
	if cmd.dir != ctxDir {
		t.Errorf("command dir = %q, want the context", cmd.dir)
	}
	if !slices.Contains(cmd.env, "KSYNC_IMAGE=api-b:ksync-build") {
		t.Errorf("command env = %v, must carry KSYNC_IMAGE with the temp tag", cmd.env)
	}
	if got := calls[1].argv; got[0] != "docker" || got[1] != "image" || got[2] != "inspect" || !slices.Contains(got, "api-b:ksync-build") {
		t.Errorf("inspect argv = %v, want docker image inspect of the temp tag", got)
	}
	if got := calls[2].argv; !slices.Equal(got, []string{"docker", "tag", "api-b:ksync-build", ref}) {
		t.Errorf("tag argv = %v, want retag from the temp name", got)
	}
}

func TestBuilder_BuildFailureStopsBeforeTagging(t *testing.T) {
	ctxDir := t.TempDir()
	b := config.Build{Image: "api-b", Context: ctxDir, Dockerfile: filepath.Join(ctxDir, "Dockerfile")}
	var calls []call
	boom := errors.New("compile error")
	builder := &Builder{
		Exec: fakeExec(&calls, "", func(argv []string) error {
			if argv[1] == "build" {
				return boom
			}
			return nil
		}),
		Output: io.Discard,
	}
	if _, err := builder.Build(context.Background(), b); !errors.Is(err, boom) {
		t.Fatalf("Build error = %v, want the build failure", err)
	}
	if len(calls) != 1 {
		t.Errorf("calls = %d, want 1 — a failed build must not tag anything", len(calls))
	}
}

// --- watch scope ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWatchScope_DefaultsToWholeContext(t *testing.T) {
	ctxDir := t.TempDir()
	dockerfile := filepath.Join(ctxDir, "Dockerfile")
	writeFile(t, dockerfile, "FROM scratch\n")
	s, err := WatchScope(config.Build{Image: "api-b", Context: ctxDir, Dockerfile: dockerfile})
	if err != nil {
		t.Fatalf("WatchScope: %v", err)
	}
	if !slices.Contains(s.Roots, ctxDir) {
		t.Errorf("Roots = %v, must contain the context", s.Roots)
	}
	if s.Ignored(filepath.Join(ctxDir, "src", "main.go")) {
		t.Error("without .dockerignore nothing may be ignored")
	}
	if s.SkipDir(filepath.Join(ctxDir, "src")) {
		t.Error("without .dockerignore no directory may be skipped")
	}
}

func TestWatchScope_DockerignoreScopesWatching(t *testing.T) {
	ctxDir := t.TempDir()
	dockerfile := filepath.Join(ctxDir, "Dockerfile")
	writeFile(t, dockerfile, "FROM scratch\n")
	writeFile(t, filepath.Join(ctxDir, ".dockerignore"),
		"**/target\n*.md\n!keep.md\nout\n!out/keep\nDockerfile\n")

	s, err := WatchScope(config.Build{Image: "api-b", Context: ctxDir, Dockerfile: dockerfile})
	if err != nil {
		t.Fatalf("WatchScope: %v", err)
	}

	ignored := []string{
		filepath.Join(ctxDir, "target", "debug", "api-b"),
		filepath.Join(ctxDir, "sub", "target", "x"),
		filepath.Join(ctxDir, "README.md"),
	}
	for _, p := range ignored {
		if !s.Ignored(p) {
			t.Errorf("Ignored(%s) = false, want true", p)
		}
	}
	watched := []string{
		filepath.Join(ctxDir, "src", "main.rs"),
		filepath.Join(ctxDir, "keep.md"),       // negation
		dockerfile,                             // always image-relevant, even when dockerignored
		filepath.Join(ctxDir, ".dockerignore"), // editing it changes the context
		"/elsewhere/unrelated",                 // outside the context
	}
	for _, p := range watched {
		if s.Ignored(p) {
			t.Errorf("Ignored(%s) = true, want false", p)
		}
	}

	if !s.SkipDir(filepath.Join(ctxDir, "target")) {
		t.Error("SkipDir(target) = false, want true (fully excluded)")
	}
	if s.SkipDir(filepath.Join(ctxDir, "out")) {
		t.Error("SkipDir(out) = true, want false (a negation re-includes out/keep)")
	}
	if s.SkipDir(filepath.Join(ctxDir, "src")) {
		t.Error("SkipDir(src) = true, want false (not excluded)")
	}
	if s.SkipDir(ctxDir) {
		t.Error("SkipDir(context root) must always be false")
	}

	for _, want := range []string{dockerfile, filepath.Join(ctxDir, ".dockerignore")} {
		if !slices.Contains(s.Roots, want) {
			t.Errorf("Roots = %v, must contain %s", s.Roots, want)
		}
	}
}

func TestWatchScope_DockerfileSpecificIgnoreFileWins(t *testing.T) {
	ctxDir := t.TempDir()
	dockerfile := filepath.Join(ctxDir, "Dockerfile")
	writeFile(t, dockerfile, "FROM scratch\n")
	writeFile(t, filepath.Join(ctxDir, ".dockerignore"), "b\n")
	writeFile(t, dockerfile+".dockerignore", "a\n")

	s, err := WatchScope(config.Build{Image: "api-b", Context: ctxDir, Dockerfile: dockerfile})
	if err != nil {
		t.Fatalf("WatchScope: %v", err)
	}
	if !s.Ignored(filepath.Join(ctxDir, "a")) {
		t.Error("Dockerfile.dockerignore must take precedence (a ignored)")
	}
	if s.Ignored(filepath.Join(ctxDir, "b")) {
		t.Error(".dockerignore must be unused when Dockerfile.dockerignore exists (b watched)")
	}
}

func TestWatchScope_ExplicitWatchPathsNarrowRoots(t *testing.T) {
	ctxDir := t.TempDir()
	srcDir := filepath.Join(ctxDir, "src")
	s, err := WatchScope(config.Build{
		Image:   "api-b",
		Context: ctxDir,
		Command: "just build",
		Watch:   []string{srcDir, filepath.Join(ctxDir, "Cargo.toml")},
	})
	if err != nil {
		t.Fatalf("WatchScope: %v", err)
	}
	if slices.Contains(s.Roots, ctxDir) {
		t.Errorf("Roots = %v: explicit watch paths must replace the context root", s.Roots)
	}
	for _, want := range []string{srcDir, filepath.Join(ctxDir, "Cargo.toml")} {
		if !slices.Contains(s.Roots, want) {
			t.Errorf("Roots = %v, must contain %s", s.Roots, want)
		}
	}
}
