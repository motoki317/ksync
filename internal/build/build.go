// Package build turns local sources into content-addressed dev images and
// derives what to watch for each build. Design rationale:
// docs/ADR/20260612-build-integration.md.
package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/motoki317/ksync/internal/config"
)

// ExecFunc runs argv in dir with extra environment variables appended to the
// inherited environment. Process stdout goes to stdout, stderr to stderr.
// Injected so Builder tests run without docker.
type ExecFunc func(ctx context.Context, dir string, env []string, argv []string, stdout, stderr io.Writer) error

// Builder produces images in the local docker daemon.
type Builder struct {
	// Exec defaults to running real processes.
	Exec ExecFunc
	// Output receives build progress; defaults to os.Stderr (docker itself
	// reports progress on stderr).
	Output io.Writer
}

// tempTag is where command builds must leave their result ($KSYNC_IMAGE); the
// content-addressed tag replaces it immediately after, so the temp tag always
// points at the latest command-build output.
const tempTag = "ksync-build"

// Build produces the image for b and returns the content-addressed ref
// <image>:ksync-<12 hex of the image fingerprint>. Unchanged inputs hit the
// docker layer cache and yield the same fingerprint, hence the same ref — a
// no-change rebuild causes no manifest change and no rollout.
func (bd *Builder) Build(ctx context.Context, b config.Build) (string, error) {
	execFn := bd.Exec
	if execFn == nil {
		execFn = runProcess
	}
	out := bd.Output
	if out == nil {
		out = os.Stderr
	}

	tmpRef := b.Image + ":" + tempTag
	if b.Command == "" {
		// --provenance=false drops the default attestation; both it and command
		// builds leave the result at tmpRef, which contentTag then fingerprints.
		argv := []string{"docker", "build", "--provenance=false", "-t", tmpRef, "-f", b.Dockerfile, b.Context}
		if err := execFn(ctx, b.Context, nil, argv, out, out); err != nil {
			return "", fmt.Errorf("building %s: %w", b.Image, err)
		}
	} else {
		env := []string{"KSYNC_IMAGE=" + tmpRef}
		if err := execFn(ctx, b.Context, env, []string{"sh", "-c", b.Command}, out, out); err != nil {
			return "", fmt.Errorf("build command for %s: %w", b.Image, err)
		}
	}
	ref, err := contentTag(ctx, execFn, b.Context, b.Image, tmpRef, out)
	if err != nil {
		return "", fmt.Errorf("building %s: %w", b.Image, err)
	}
	return ref, nil
}

// BuildGroup runs command once to build every entry in builds together (a
// `docker buildx bake` of many targets, a host compile producing many images),
// then content-tags each result and returns the refs in builds order. command
// receives $KSYNC_IMAGES — the newline-separated temp refs (<image>:ksync-build)
// it must leave in the daemon — mirroring the single-build $KSYNC_IMAGE
// contract. It runs in the first entry's context (config guarantees the group
// shares one). Sharing one build invocation is the whole point: a common base
// image or compiler pass runs once instead of once per image.
func (bd *Builder) BuildGroup(ctx context.Context, command string, builds []config.Build) ([]string, error) {
	if len(builds) == 0 {
		return nil, nil
	}
	execFn := bd.Exec
	if execFn == nil {
		execFn = runProcess
	}
	out := bd.Output
	if out == nil {
		out = os.Stderr
	}

	dir := builds[0].Context
	tmpRefs := make([]string, len(builds))
	for i, b := range builds {
		tmpRefs[i] = b.Image + ":" + tempTag
	}
	env := []string{"KSYNC_IMAGES=" + strings.Join(tmpRefs, "\n")}
	if err := execFn(ctx, dir, env, []string{"sh", "-c", command}, out, out); err != nil {
		return nil, fmt.Errorf("build group command: %w", err)
	}

	refs := make([]string, len(builds))
	for i, b := range builds {
		ref, err := contentTag(ctx, execFn, dir, b.Image, tmpRefs[i], out)
		if err != nil {
			return nil, fmt.Errorf("build group result %s: %w", tmpRefs[i], err)
		}
		refs[i] = ref
	}
	return refs, nil
}

// fingerprintFormat asks `docker image inspect` for the two parts of an image
// that reproducible content fixes: its runtime config (Entrypoint/Cmd/Env/…)
// and its layer diffIDs. Both are stable across rebuilds of identical content;
// the config *image ID* deliberately is not used because build tools stamp
// per-build timestamps into the image config (docker buildx bake does this even
// with attestations disabled), which would give unchanged content a new ID and
// roll deployments on every build.
const fingerprintFormat = "{{json .Config}}{{json .RootFS.Layers}}"

// contentTag fingerprints the image at tmpRef and retags it to the
// content-addressed <image>:ksync-<hash>, returning that ref. It retags from
// the temp name, not the ID: under docker's containerd image store the
// build-reported ID is the config digest, which `docker tag` does not resolve.
func contentTag(ctx context.Context, execFn ExecFunc, dir, image, tmpRef string, out io.Writer) (string, error) {
	var buf strings.Builder
	argv := []string{"docker", "image", "inspect", "--format", fingerprintFormat, tmpRef}
	if err := execFn(ctx, dir, nil, argv, &buf, out); err != nil {
		return "", fmt.Errorf("inspecting %s: %w", tmpRef, err)
	}
	tag, err := devTag(buf.String())
	if err != nil {
		return "", err
	}
	ref := image + ":" + tag
	if err := execFn(ctx, dir, nil, []string{"docker", "tag", tmpRef, ref}, out, out); err != nil {
		return "", fmt.Errorf("tagging %s: %w", ref, err)
	}
	return ref, nil
}

// Tag extracts the tag part of a ref Build returned — what callers feed to
// the render-time image override (which pairs it with the bare image name).
func Tag(ref string) string {
	return ref[strings.LastIndex(ref, ":")+1:]
}

// devTag derives the dev tag from an image fingerprint (its config + layer
// diffIDs, see fingerprintFormat) by hashing it. 12 hex digits match docker's
// own short-ID display and keep collisions irrelevant at local-daemon scale.
func devTag(fingerprint string) (string, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return "", fmt.Errorf("empty image fingerprint")
	}
	sum := sha256.Sum256([]byte(fingerprint))
	return "ksync-" + hex.EncodeToString(sum[:])[:12], nil
}

func runProcess(ctx context.Context, dir string, env []string, argv []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
