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

// watch accepts image overrides with takeover semantics, so it defines the same
// --image flag `ksync sync` does (an overridden build is seeded, not built, on the
// first sync; its source is still watched so the first edit takes it over).
func TestRunWatch_DefinesImageFlag(t *testing.T) {
	if newWatchCmd().Flags().Lookup("image") == nil {
		t.Error("watch does not define --image; want it accepted (takeover semantics)")
	}
}

// A set KSYNC_IMAGE_OVERRIDES is no longer rejected wholesale: watch parses it the
// way `ksync sync` does. A malformed value surfaces the shared parse error before
// any cluster work — proof the env is read as overrides, not refused outright.
func TestRunWatch_AcceptsEnvOverrides(t *testing.T) {
	cfgPath := writeMinimalConfig(t)
	t.Setenv(overrideEnv, "not-an-assignment")

	err := executeKsync(t, "watch", "-f", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "invalid override") {
		t.Fatalf("watch did not parse KSYNC_IMAGE_OVERRIDES as sync does: err = %v", err)
	}
	if strings.Contains(err.Error(), "does not accept image overrides") {
		t.Errorf("watch still rejects overrides wholesale: %v", err)
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
