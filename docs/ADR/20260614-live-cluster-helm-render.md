---
date: "2026-06-14"
author: "motoki317"
status: "accepted"
---

# Context

ksync renders helmCharts in-process via kustomize (krusty), which shells out to `helm template`.
`helm template` runs fully offline: helm's `lookup` function returns empty, so any chart that reads
live cluster state at template time cannot be rendered — and a chart that `fail`s on a missing
lookup does not render at all.

Dogfooding a second real monorepo surfaced exactly this. Its shared `microservice` chart resolves a
Traefik Service's ClusterIP via `lookup "v1" "Service" …` (to build a pod `hostAliases` entry) and
`fail`s when the lookup returns nothing — 17 of its release values use it, including the core API
and auth services. Offline rendering (ksync, and equally `kustomize build`, ArgoCD, Flux)
cannot satisfy it; only helm's install path (`helm upgrade --install`, which the project drives via
helmfile) has cluster access. So ksync could render only the chart's lookup-free services — the full
app layer was unrenderable.

This is not a niche chart bug: install-time `lookup` is a common "helm but not GitOps" pattern, and
ksync's whole reason to exist is local development against a live cluster it is *already connected
to*. Unlike a CI/GitOps renderer, ksync always has the target cluster in hand.

# Decision

Render helmCharts against the live cluster **by default** (opt out with `--offline-render`). Because
ksync only ever runs against one explicitly allowlisted local context, the cluster is always
available, so the cost that makes live rendering unacceptable for offline GitOps tools does not
apply here.

Mechanism: ksync hands kustomize a tiny `helm` wrapper as its `HelmConfig.Command`. For the
`template` subcommand the wrapper appends

    --dry-run=server --take-ownership --kube-context <ksync's context>
    --kube-version <server version> --api-versions <each discovered group/version[/Kind]>

and passes every other helm subcommand (`version`, `pull`, …) straight through.

- `--dry-run=server` is the only mode in which `lookup` reaches the cluster; it is a read-only
  server-side simulation (no writes).
- `--take-ownership` skips the install-time check that existing objects carry Helm's ownership
  labels. ksync's resources are SSA-managed under its own tracking label, not Helm's, so without it
  the server dry-run aborts with "cannot be imported into the current release" the moment an app is
  already deployed.
- `--kube-version` / `--api-versions` feed helm the cluster's real capabilities, so a version-gated
  template (a PDB guarded by `.Capabilities.APIVersions.Has "policy/v1/PodDisruptionBudget"`)
  renders for the actual target. **Helm v3 does not backfill `.Capabilities` from `--dry-run=server`
  — only helm v4 does** — so without this a v3 dev shell silently rendered a removed apiVersion
  (`policy/v1beta1`) that then failed to apply. ksync discovers the set once per command
  (`engine.DiscoverCapabilities`); the api-versions list is long, so the wrapper reads it from a
  file rather than from baked-in args. This matches what ArgoCD passes to helm.

The real helm path and the context are passed to the wrapper through environment variables, never
interpolated into the script, so a context name cannot become shell injection. The wrapper lives
only in `cmd/ksync`; `internal/render` is unchanged and still defaults to plain offline `helm`,
which keeps its byte-parity-with-`kustomize build` test honest (that test renders without a
cluster).

# Consequences

- Charts that read live cluster state at template time render correctly, so ksync can drive stacks
  built around install-time `lookup` — the second monorepo's full app layer now renders, with
  `hostAliases` resolved to the real Traefik ClusterIP.
- For charts that do **not** use `lookup`, live rendering is transparent: the output is
  byte-identical to offline (verified by diffing a live vs `--offline-render` render — zero
  difference, no helm ownership metadata leaks into the manifests). So enabling it by default
  changes nothing for the common case.
- `--offline-render` restores pure offline rendering for a quick `ksync render` with no cluster, or
  to shave the latency below.

# Impact

- Each helm render now makes cluster round-trips (capabilities + lookups): measured ~0.47s vs ~0.19s
  offline for a 26-object chart, ~0.28s added. It runs once per change and is dwarfed by build and
  cluster-load, so the inner loop is unaffected; `--offline-render` is the escape hatch if needed.
- Rendering now depends on cluster connectivity by default. Acceptable because ksync already
  requires it to sync, and refuses to run against anything but its one allowlisted context.
- The helm binary must be on PATH (it already is for any helmCharts user); a clear error points to
  `--offline-render` if not.

# Alternatives

- **Keep offline-only; tell users to make charts GitOps-renderable.** Rejected: pushes a real
  rewrite onto every chart using `lookup`, for no benefit to a tool that is *defined* by having the
  live cluster available.
- **`--dry-run=server` without `--take-ownership`.** Rejected: aborts on the helm-ownership check
  for any resource ksync already manages, so it breaks on the second sync of every helm app.
- **Plain `helm template --kube-context …`.** Does not enable `lookup` (verified: returns empty);
  only `--dry-run=server` does.
- **Opt-in instead of default.** Rejected per the local-only design: ksync always has the cluster,
  so live rendering is the correct default and the rare offline case is the flag.
