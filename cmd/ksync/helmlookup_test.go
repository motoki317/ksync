package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHelm writes a stand-in helm that prints its arguments, so the wrapper's
// rewriting can be asserted without a real helm or cluster.
func fakeHelm(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-helm")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func runWrapper(t *testing.T, args ...string) []string {
	t.Helper()
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "helm")
	if err := os.WriteFile(wrapper, []byte(helmLookupWrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapper, args...)
	cmd.Env = append(os.Environ(), "KSYNC_HELM="+fakeHelm(t), "KSYNC_KUBE_CONTEXT=dev-ctx")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("wrapper %v: %v", args, err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestHelmLookupWrapper_TemplateGetsLiveFlags(t *testing.T) {
	got := runWrapper(t, "template", "ns", "chart", "--namespace", "ns-system")
	want := []string{"template", "ns", "chart", "--namespace", "ns-system", "--dry-run=server", "--take-ownership", "--kube-context", "dev-ctx"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("template args:\n got %v\nwant %v", got, want)
	}
}

func TestHelmLookupWrapper_OtherSubcommandsPassThrough(t *testing.T) {
	// version/pull must not gain the template-only flags (and must not contact a
	// cluster), or kustomize's helm-version probe and chart pulls would break.
	for _, sub := range [][]string{{"version", "--short"}, {"pull", "chart", "--version", "1.0.0"}} {
		got := runWrapper(t, sub...)
		if strings.Join(got, " ") != strings.Join(sub, " ") {
			t.Errorf("%q rewritten to %v; pass-through expected", sub, got)
		}
	}
}

// With the cluster version and api-versions file set, template gets
// --kube-version and one --api-versions per line — so version-gated chart
// templates resolve against the real cluster even on helm v3.
func TestHelmLookupWrapper_TemplateGetsCapabilityFlags(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "helm")
	if err := os.WriteFile(wrapper, []byte(helmLookupWrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	apiFile := filepath.Join(dir, "api-versions")
	if err := os.WriteFile(apiFile, []byte("policy/v1\npolicy/v1/PodDisruptionBudget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapper, "template", "chart")
	cmd.Env = append(os.Environ(),
		"KSYNC_HELM="+fakeHelm(t),
		"KSYNC_KUBE_CONTEXT=dev-ctx",
		"KSYNC_KUBE_VERSION=v1.30.2",
		"KSYNC_API_VERSIONS="+apiFile,
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	got := strings.Join(strings.Fields(string(out)), " ")
	for _, want := range []string{
		"--kube-version v1.30.2",
		"--api-versions policy/v1",
		"--api-versions policy/v1/PodDisruptionBudget",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("template args %q missing %q", got, want)
		}
	}
}

func TestRenderOptions_OfflineUsesPlainHelm(t *testing.T) {
	opts, cleanup, err := renderOptions("dev-ctx", true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	// Offline must leave HelmCommand empty so the renderer uses plain `helm`
	// (no cluster contact) — the byte-parity contract with `kustomize build`.
	if opts.HelmCommand != "" {
		t.Errorf("offline render set HelmCommand=%q; want empty", opts.HelmCommand)
	}
}
