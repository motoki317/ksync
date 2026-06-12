// Package build turns local sources into content-addressed dev images and
// derives what to watch for each build. Design rationale:
// docs/ADR/20260612-build-integration.md.
package build

import (
	"context"
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
// <image>:ksync-<12 hex of the image ID>. Unchanged inputs hit the docker
// layer cache and yield the same ID, hence the same ref — a no-change rebuild
// causes no manifest change and no rollout.
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
	var id string
	if b.Command == "" {
		iidFile, err := os.CreateTemp("", "ksync-iid-*")
		if err != nil {
			return "", err
		}
		iidPath := iidFile.Name()
		_ = iidFile.Close()
		defer func() { _ = os.Remove(iidPath) }()
		// --provenance=false: the default provenance attestation embeds build
		// timestamps, giving identical content a new image ID on every build —
		// which would defeat content-addressed tags and roll deployments on
		// no-change rebuilds (verified against docker desktop's containerd
		// image store).
		argv := []string{"docker", "build", "--provenance=false", "-t", tmpRef, "-f", b.Dockerfile, "--iidfile", iidPath, b.Context}
		if err := execFn(ctx, b.Context, nil, argv, out, out); err != nil {
			return "", fmt.Errorf("building %s: %w", b.Image, err)
		}
		raw, err := os.ReadFile(iidPath)
		if err != nil {
			return "", fmt.Errorf("building %s: reading the image ID: %w", b.Image, err)
		}
		id = strings.TrimSpace(string(raw))
	} else {
		env := []string{"KSYNC_IMAGE=" + tmpRef}
		if err := execFn(ctx, b.Context, env, []string{"sh", "-c", b.Command}, out, out); err != nil {
			return "", fmt.Errorf("build command for %s: %w", b.Image, err)
		}
		var buf strings.Builder
		argv := []string{"docker", "image", "inspect", "--format", "{{.Id}}", tmpRef}
		if err := execFn(ctx, b.Context, nil, argv, &buf, out); err != nil {
			return "", fmt.Errorf("build command for %s did not produce %s in the docker daemon: %w", b.Image, tmpRef, err)
		}
		id = strings.TrimSpace(buf.String())
	}

	tag, err := devTag(id)
	if err != nil {
		return "", fmt.Errorf("building %s: %w", b.Image, err)
	}
	ref := b.Image + ":" + tag
	// Retag from the temp name, not the ID: under docker's containerd image
	// store the build-reported ID is the config digest, which `docker tag`
	// does not resolve.
	if err := execFn(ctx, b.Context, nil, []string{"docker", "tag", tmpRef, ref}, out, out); err != nil {
		return "", fmt.Errorf("tagging %s: %w", ref, err)
	}
	return ref, nil
}

// Tag extracts the tag part of a ref Build returned — what callers feed to
// the render-time image override (which pairs it with the bare image name).
func Tag(ref string) string {
	return ref[strings.LastIndex(ref, ":")+1:]
}

// devTag derives the dev tag from a docker image ID ("sha256:<64 hex>"). 12
// hex digits match docker's own short-ID display and keep collisions
// irrelevant at local-daemon scale.
func devTag(imageID string) (string, error) {
	id := strings.TrimPrefix(strings.TrimSpace(imageID), "sha256:")
	if len(id) < 12 {
		return "", fmt.Errorf("unexpected image ID %q", imageID)
	}
	return "ksync-" + id[:12], nil
}

func runProcess(ctx context.Context, dir string, env []string, argv []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
