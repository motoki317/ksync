package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/motoki317/ksync/internal/render"
)

// helmLookupWrapper is a drop-in `helm` shim handed to kustomize's helmCharts
// inflation. For `template` it adds the flags that make `lookup` resolve against
// the live cluster; every other helm subcommand (version, pull, …) passes
// straight through. ksync only ever runs against a local dev cluster, so
// rendering against it is always available and is the default — it is what lets
// charts that read live Services at template time (a common helm-but-not-GitOps
// pattern) render at all.
//
//   - --dry-run=server: simulate server-side (no writes), which is the only mode
//     in which helm's `lookup` reaches the cluster (plain `helm template`
//     returns empty and a chart that `fail`s on a missing lookup never renders).
//   - --take-ownership: skip the install-time check that existing objects carry
//     Helm's ownership labels. ksync's resources are SSA-managed under its own
//     tracking label, not Helm's, so without this the server dry-run aborts with
//     "cannot be imported into the current release" once an app is deployed.
//
// The real helm path and the context arrive via env vars, never interpolated
// into the script, so a context name can carry any character without becoming
// shell injection. ksync sets both in its own process; kustomize's exec
// inherits them.
const helmLookupWrapper = `#!/bin/sh
if [ "$1" = "template" ]; then
	shift
	exec "$KSYNC_HELM" template "$@" --dry-run=server --take-ownership --kube-context "$KSYNC_KUBE_CONTEXT"
fi
exec "$KSYNC_HELM" "$@"
`

// setupHelmLookup writes the wrapper to a temp dir, points ksync's env at the
// real helm and the given context, and returns the wrapper path to use as the
// renderer's HelmCommand (plus a cleanup). Returns an error only if helm is
// missing or the temp file cannot be written; callers fall back to offline
// rendering by passing --offline-render.
func setupHelmLookup(kubeContext string) (helmCommand string, cleanup func(), err error) {
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		return "", nil, fmt.Errorf("helm not found on PATH (needed to render charts against the cluster; use --offline-render to skip): %w", err)
	}
	dir, err := os.MkdirTemp("", "ksync-helm-")
	if err != nil {
		return "", nil, err
	}
	// Named "helm" so kustomize's own `helm version` probe and any log line read
	// naturally; the basename is otherwise irrelevant.
	wrapper := filepath.Join(dir, "helm")
	if err := os.WriteFile(wrapper, []byte(helmLookupWrapper), 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	if err := os.Setenv("KSYNC_HELM", helmPath); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	if err := os.Setenv("KSYNC_KUBE_CONTEXT", kubeContext); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return wrapper, func() { _ = os.RemoveAll(dir) }, nil
}

// renderOptions builds the renderer options for a command: live-cluster helm
// lookups against cfg's context unless offline is set. cleanup removes the
// wrapper temp dir (a no-op when offline).
func renderOptions(kubeContext string, offline bool) (render.Options, func(), error) {
	if offline {
		return render.Options{}, func() {}, nil
	}
	helmCmd, cleanup, err := setupHelmLookup(kubeContext)
	if err != nil {
		return render.Options{}, func() {}, err
	}
	return render.Options{HelmCommand: helmCmd}, cleanup, nil
}
