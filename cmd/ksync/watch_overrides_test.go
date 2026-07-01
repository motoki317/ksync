package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motoki317/ksync/internal/engine"
)

// executeKsync runs the real cobra command tree with args and returns the command
// error, so a test exercises flag parsing and dispatch exactly as the binary does.
// Output is discarded (the root already silences cobra's own error/usage printing).
func executeKsync(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

// watch rebuilds from source, so it must refuse image overrides outright rather
// than silently ignore them: a set KSYNC_IMAGE_OVERRIDES fails the command fast,
// before it touches the cluster.
func TestRunWatch_RejectsEnvOverrides(t *testing.T) {
	cfgPath := writeMinimalConfig(t)
	t.Setenv(overrideEnv, "ghcr.io/org/api-b=prebuilt-tag")

	err := executeKsync(t, "watch", "-f", cfgPath)
	if err == nil {
		t.Fatal("watch accepted a set KSYNC_IMAGE_OVERRIDES; want a fail-fast error")
	}
	if !strings.Contains(err.Error(), "does not accept image overrides") {
		t.Errorf("error %q does not explain that watch rejects overrides", err)
	}
}

// The --image flag is a sync-only affordance; watch must not even define it, so
// passing it is a flag error (not a silently-accepted override).
func TestRunWatch_RejectsImageFlag(t *testing.T) {
	err := executeKsync(t, "watch", "--image", "ghcr.io/org/api-b=prebuilt-tag")
	if err == nil {
		t.Fatal("watch accepted --image; want it undefined on watch")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("error %q does not name the rejected flag", err)
	}
}

// The live health-gate line names the first few not-ready resources by short
// name, with a "+N" once the list overflows, so the developer sees what the
// deploy is waiting on without an unbounded line.
func TestWaitingTail(t *testing.T) {
	rs := func(kind, name string) engine.ResourceStatus {
		return engine.ResourceStatus{Kind: kind, Name: name, Status: "Progressing"}
	}
	cases := []struct {
		name    string
		pending []engine.ResourceStatus
		want    string
	}{
		{"none", nil, "waiting for health  0 not ready"},
		{"one", []engine.ResourceStatus{rs("Deployment", "api")}, "waiting for health  1 not ready: Deployment/api"},
		{
			"overflow",
			[]engine.ResourceStatus{rs("Deployment", "api"), rs("StatefulSet", "db"), rs("Job", "migrate"), rs("Pod", "worker")},
			"waiting for health  4 not ready: Deployment/api, StatefulSet/db, Job/migrate, +1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := waitingTail(c.pending); got != c.want {
				t.Errorf("waitingTail = %q, want %q", got, c.want)
			}
		})
	}
}

// writeMinimalConfig writes a valid ksync.yaml (one app over a kustomize dir) to
// a temp dir and returns its path — enough for runWatch's load to succeed so the
// override guard, which runs right after, is what the test exercises.
func writeMinimalConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	appDir := filepath.Join(dir, "apps", "api-b")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "kustomization.yaml"), []byte("resources: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "ksync.yaml")
	if err := os.WriteFile(cfgPath, []byte("allowedContexts: [docker-desktop]\napps:\n  - path: apps/api-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}
