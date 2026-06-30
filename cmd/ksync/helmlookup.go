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

// helmNoLookupWrapper is the wrapper for an app rendered with `clientRender:
// true`: the lookup wrapper minus `--dry-run=server --take-ownership
// --kube-context`. Dropping the server dry-run is the whole point — it stops the
// apiserver from validating every rendered resource, so an app whose chart ships
// a CRD together with custom resources of that kind renders before the CRD
// exists (the default wrapper fails it with "no matches for kind"). It keeps
// --kube-version / --api-versions, so capabilities still match the live cluster
// — the way ArgoCD renders. The cost is that `helm lookup` no longer resolves
// (it needs the server dry-run), which is why this is opt-in per app.
//
// A separate file, not the lookup wrapper toggled by an env var: app renders run
// concurrently and share this process's env, so a per-render env toggle would
// race. The choice of wrapper is made by which path kustomize is handed.
const helmNoLookupWrapper = `#!/bin/sh
if [ -n "$KSYNC_HELM_UNAVAILABLE" ]; then
	echo "ksync: cannot render helm charts: $KSYNC_HELM_UNAVAILABLE" >&2
	echo "ksync: install helm and ensure the cluster is reachable, or pass --offline-render" >&2
	exit 1
fi
if [ "$1" = "template" ]; then
	shift
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

// setupHelm writes both helm wrappers — the default lookup wrapper and the
// no-lookup wrapper (clientRender) — to a temp dir, points ksync's env at the
// real helm, the context, and the discovered capabilities, and returns both
// wrapper paths (plus a cleanup). The two wrappers share this env; which one a
// chart is inflated with is decided per app by the path the renderer hands
// kustomize. Each lives in its own subdir so both keep the basename "helm", so
// kustomize's `helm version` probe and its log lines read naturally.
//
// Missing helm or an unreachable cluster is NOT a setup failure: a pure-kustomize
// app never execs a wrapper, so it must render without either. Instead the
// failure reason is stashed in KSYNC_HELM_UNAVAILABLE, and the wrapper surfaces it
// only if kustomize actually inflates a chart — deferring the error to where it
// matters, rather than gating every render on helm and cluster connectivity.
// Returns an error only if the temp files cannot be written.
func setupHelm(kubeContext string) (lookupCommand, clientCommand string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "ksync-helm-")
	if err != nil {
		return "", "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	for _, w := range []struct {
		sub    string
		script string
		dst    *string
	}{
		{"lookup", helmLookupWrapper, &lookupCommand},
		{"client", helmNoLookupWrapper, &clientCommand},
	} {
		subdir := filepath.Join(dir, w.sub)
		if err := os.MkdirAll(subdir, 0o755); err != nil {
			cleanup()
			return "", "", nil, err
		}
		p := filepath.Join(subdir, "helm")
		if err := os.WriteFile(p, []byte(w.script), 0o755); err != nil {
			cleanup()
			return "", "", nil, err
		}
		*w.dst = p
	}

	env := map[string]string{"KSYNC_KUBE_CONTEXT": kubeContext}
	// Resolve helm and the cluster's capabilities, but defer any failure to chart
	// inflation. Capabilities are discovered up front (once per command) so a
	// version-gated chart template (a PDB behind a Capabilities.APIVersions.Has)
	// resolves against the real cluster; on a discovery failure the wrappers refuse
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
				return "", "", nil, err
			}
			env["KSYNC_KUBE_VERSION"] = kubeVersion
			env["KSYNC_API_VERSIONS"] = apiVersionsFile
		}
	}
	// Always set it (empty when available) so an inherited stale value can never
	// make a wrapper wrongly refuse a chart.
	env["KSYNC_HELM_UNAVAILABLE"] = unavailable
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			cleanup()
			return "", "", nil, err
		}
	}
	return lookupCommand, clientCommand, cleanup, nil
}

// renderOptions builds the renderer options for a command: live-cluster helm
// lookups against cfg's context unless offline is set, plus the no-lookup
// command an app with clientRender uses. cleanup removes the wrapper temp dir (a
// no-op when offline, where every app renders with plain helm and no cluster).
func renderOptions(kubeContext string, offline bool) (render.Options, func(), error) {
	if offline {
		return render.Options{}, func() {}, nil
	}
	lookupCmd, clientCmd, cleanup, err := setupHelm(kubeContext)
	if err != nil {
		return render.Options{}, func() {}, err
	}
	return render.Options{HelmCommand: lookupCmd, ClientRenderCommand: clientCmd}, cleanup, nil
}
