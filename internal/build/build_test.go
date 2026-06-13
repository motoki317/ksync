package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	fp := `{"Entrypoint":["/app"]}["sha256:aaa","sha256:bbb"]`
	tag, err := devTag(fp)
	if err != nil {
		t.Fatalf("devTag: %v", err)
	}
	if want := "ksync-" + fpHash(fp); tag != want {
		t.Errorf("devTag = %q, want %q", tag, want)
	}
	// Deterministic for identical content, changes when content changes — the
	// property that makes unchanged rebuilds not roll pods.
	if t2, _ := devTag(fp); t2 != tag {
		t.Error("devTag is not deterministic")
	}
	if t3, _ := devTag(fp + "x"); t3 == tag {
		t.Error("devTag did not change when the fingerprint changed")
	}
	if _, err := devTag("   "); err == nil {
		t.Error("devTag accepted an empty fingerprint")
	}
}

// fpHash mirrors devTag's hashing so tests can assert the exact dev tag.
func fpHash(fingerprint string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(fingerprint)))
	return hex.EncodeToString(sum[:])[:12]
}

type call struct {
	dir  string
	env  []string
	argv []string
}

// fakeExec scripts process execution: it answers `docker image inspect` (the
// fingerprint query) with the given fingerprint string, which ksync hashes into
// the dev tag.
func fakeExec(calls *[]call, fingerprint string, failOn func(argv []string) error) ExecFunc {
	return func(_ context.Context, dir string, env []string, argv []string, stdout, _ io.Writer) error {
		*calls = append(*calls, call{dir: dir, env: env, argv: argv})
		if failOn != nil {
			if err := failOn(argv); err != nil {
				return err
			}
		}
		if argv[0] == "docker" && argv[1] == "image" && argv[2] == "inspect" {
			_, _ = io.WriteString(stdout, fingerprint+"\n")
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
	fingerprint := `{"Entrypoint":["/app/api-b"]}["sha256:1111","sha256:2222"]`
	var calls []call
	builder := &Builder{Exec: fakeExec(&calls, fingerprint, nil), Output: io.Discard}

	ref, err := builder.Build(context.Background(), b)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "example.com/team-a/api-b:ksync-" + fpHash(fingerprint); ref != want {
		t.Errorf("ref = %q, want %q", ref, want)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3 (build, inspect, tag)", len(calls))
	}
	bld := calls[0]
	if bld.dir != ctxDir {
		t.Errorf("build dir = %q, want the context", bld.dir)
	}
	// --provenance=false drops the default attestation. ksync no longer trusts
	// the image ID for content addressing (it drifts), so there is no --iidfile.
	for _, want := range []string{"docker", "build", "--provenance=false", "-f", b.Dockerfile, ctxDir} {
		if !slices.Contains(bld.argv, want) {
			t.Errorf("build argv %v missing %q", bld.argv, want)
		}
	}
	if slices.Contains(bld.argv, "--iidfile") {
		t.Errorf("build argv %v must not use --iidfile (ksync fingerprints the built image instead)", bld.argv)
	}
	// Retagging must go through the temp name: the containerd image store
	// does not resolve config digests in `docker tag`.
	tmpRef := "example.com/team-a/api-b:ksync-build"
	if !slices.Contains(bld.argv, tmpRef) {
		t.Errorf("build argv %v missing temp tag %q", bld.argv, tmpRef)
	}
	if got := calls[1].argv; got[0] != "docker" || got[1] != "image" || got[2] != "inspect" || !slices.Contains(got, tmpRef) {
		t.Errorf("inspect argv = %v, want docker image inspect of the temp tag", got)
	}
	if got := calls[2].argv; !slices.Equal(got, []string{"docker", "tag", tmpRef, ref}) {
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
	fingerprint := `{"Entrypoint":["/api-b"]}["sha256:efef"]`
	var calls []call
	builder := &Builder{Exec: fakeExec(&calls, fingerprint, nil), Output: io.Discard}

	ref, err := builder.Build(context.Background(), b)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "api-b:ksync-" + fpHash(fingerprint); ref != want {
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

func TestBuilder_BuildGroup(t *testing.T) {
	ctxDir := t.TempDir()
	builds := []config.Build{
		{Image: "ghcr.io/team-a/api-b", Context: ctxDir, Group: "g"},
		{Image: "ghcr.io/team-a/api-c", Context: ctxDir, Group: "g"},
	}
	fingerprint := `{"Entrypoint":["/srv"]}["sha256:abab"]`
	var calls []call
	builder := &Builder{Exec: fakeExec(&calls, fingerprint, nil), Output: io.Discard}

	refs, err := builder.BuildGroup(context.Background(), "docker buildx bake $KSYNC_IMAGES", builds)
	if err != nil {
		t.Fatalf("BuildGroup: %v", err)
	}
	// Both images share the fake fingerprint, so they share a dev tag — valid,
	// since they are distinct repositories (NeoShowcase's components likewise
	// share layers and differ only by image name).
	tag := "ksync-" + fpHash(fingerprint)
	want := []string{"ghcr.io/team-a/api-b:" + tag, "ghcr.io/team-a/api-c:" + tag}
	if !slices.Equal(refs, want) {
		t.Errorf("refs = %v, want %v", refs, want)
	}
	// One bulk command + per image (inspect, tag): 1 + 2*2 = 5 calls.
	if len(calls) != 5 {
		t.Fatalf("calls = %d, want 5 (1 command + 2x(inspect, tag))", len(calls))
	}
	cmd := calls[0]
	if !slices.Equal(cmd.argv, []string{"sh", "-c", "docker buildx bake $KSYNC_IMAGES"}) {
		t.Errorf("command argv = %v", cmd.argv)
	}
	if cmd.dir != ctxDir {
		t.Errorf("command dir = %q, want the shared context", cmd.dir)
	}
	// The command learns its targets from $KSYNC_IMAGES — the newline-separated
	// temp refs it must produce — not $KSYNC_IMAGE.
	wantImages := "KSYNC_IMAGES=ghcr.io/team-a/api-b:ksync-build\nghcr.io/team-a/api-c:ksync-build"
	if !slices.Contains(cmd.env, wantImages) {
		t.Errorf("command env = %v, must carry %q", cmd.env, wantImages)
	}
	// Each image is content-tagged from its own temp name.
	if got := calls[2].argv; !slices.Equal(got, []string{"docker", "tag", "ghcr.io/team-a/api-b:ksync-build", want[0]}) {
		t.Errorf("api-b tag argv = %v", got)
	}
	if got := calls[4].argv; !slices.Equal(got, []string{"docker", "tag", "ghcr.io/team-a/api-c:ksync-build", want[1]}) {
		t.Errorf("api-c tag argv = %v", got)
	}
}

func TestBuilder_BuildGroupCommandFailureStopsBeforeTagging(t *testing.T) {
	ctxDir := t.TempDir()
	builds := []config.Build{{Image: "api-b", Context: ctxDir, Group: "g"}}
	var calls []call
	boom := errors.New("bake failed")
	builder := &Builder{
		Exec: fakeExec(&calls, "", func(argv []string) error {
			if argv[0] == "sh" {
				return boom
			}
			return nil
		}),
		Output: io.Discard,
	}
	if _, err := builder.BuildGroup(context.Background(), "bake", builds); !errors.Is(err, boom) {
		t.Fatalf("BuildGroup error = %v, want the command failure", err)
	}
	if len(calls) != 1 {
		t.Errorf("calls = %d, want 1 — a failed bulk command must not inspect or tag", len(calls))
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
