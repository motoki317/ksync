package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/motoki317/ksync/internal/engine"
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
//   - --kube-version / --api-versions: the cluster's real Kubernetes version and
//     API set, so version-gated templates (a PDB's
//     `.Capabilities.APIVersions.Has "policy/v1/PodDisruptionBudget"`) resolve
//     for the actual target. Helm v3 does NOT fill these from --dry-run=server —
//     without them it leaves Capabilities at static defaults and a chart silently
//     renders a removed apiVersion that fails to apply. The api-versions list is
//     long, so it is read from a file (one per line) rather than baked into args.
//
// The real helm path, the context, the version, and the api-versions file all
// arrive via env vars, never interpolated into the script, so any value can
// carry any character without becoming shell injection. ksync sets them in its
// own process; kustomize's exec inherits them.
//
// KSYNC_HELM_UNAVAILABLE defers the "helm missing or cluster unreachable" error
// to the moment kustomize actually inflates a chart: a pure-kustomize app never
// execs this wrapper, so it renders with neither helm nor a reachable cluster.
const helmLookupWrapper = `#!/bin/sh
if [ -n "$KSYNC_HELM_UNAVAILABLE" ]; then
	echo "ksync: cannot render helm charts: $KSYNC_HELM_UNAVAILABLE" >&2
	echo "ksync: install helm and ensure the cluster is reachable, or pass --offline-render" >&2
	exit 1
fi
if [ "$1" = "template" ]; then
	shift
	set -- "$@" --dry-run=server --take-ownership --kube-context "$KSYNC_KUBE_CONTEXT"
	[ -n "$KSYNC_KUBE_VERSION" ] && set -- "$@" --kube-version "$KSYNC_KUBE_VERSION"
	if [ -n "$KSYNC_API_VERSIONS" ] && [ -f "$KSYNC_API_VERSIONS" ]; then
		while IFS= read -r v; do
			[ -n "$v" ] && set -- "$@" --api-versions "$v"
		done < "$KSYNC_API_VERSIONS"
	fi
	exec "$KSYNC_HELM" template "$@"
fi
exec "$KSYNC_HELM" "$@"
`

// setupHelmLookup writes the wrapper to a temp dir, points ksync's env at the
// real helm and the given context, and returns the wrapper path to use as the
// renderer's HelmCommand (plus a cleanup).
//
// Missing helm or an unreachable cluster is NOT a setup failure: a pure-kustomize
// app never execs the wrapper, so it must render without either. Instead the
// failure reason is stashed in KSYNC_HELM_UNAVAILABLE, and the wrapper surfaces it
// only if kustomize actually inflates a chart — deferring the error to where it
// matters, rather than gating every render on helm and cluster connectivity.
// Returns an error only if the temp files cannot be written.
func setupHelmLookup(kubeContext string) (helmCommand string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "ksync-helm-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	// Named "helm" so kustomize's own `helm version` probe and any log line read
	// naturally; the basename is otherwise irrelevant.
	wrapper := filepath.Join(dir, "helm")
	if err := os.WriteFile(wrapper, []byte(helmLookupWrapper), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}

	env := map[string]string{"KSYNC_KUBE_CONTEXT": kubeContext}
	// Resolve helm and the cluster's capabilities, but defer any failure to chart
	// inflation. Capabilities are discovered up front (once per command) so a
	// version-gated chart template (a PDB behind a Capabilities.APIVersions.Has)
	// resolves against the real cluster; on a discovery failure the wrapper refuses
	// to render charts rather than rendering them with wrong (static) capabilities.
	unavailable := ""
	if helmPath, lookErr := exec.LookPath("helm"); lookErr != nil {
		unavailable = "helm not found on PATH"
	} else {
		env["KSYNC_HELM"] = helmPath
		apiVersions, kubeVersion, capErr := engine.DiscoverCapabilities(kubeContext)
		if capErr != nil {
			unavailable = fmt.Sprintf("reading cluster capabilities: %v", capErr)
		} else {
			apiVersionsFile := filepath.Join(dir, "api-versions")
			if err := os.WriteFile(apiVersionsFile, []byte(strings.Join(apiVersions, "\n")), 0o644); err != nil {
				cleanup()
				return "", nil, err
			}
			env["KSYNC_KUBE_VERSION"] = kubeVersion
			env["KSYNC_API_VERSIONS"] = apiVersionsFile
		}
	}
	// Always set it (empty when available) so an inherited stale value can never
	// make the wrapper wrongly refuse a chart.
	env["KSYNC_HELM_UNAVAILABLE"] = unavailable
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return wrapper, cleanup, nil
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
