---
date: "2026-06-12"
author: "motoki317"
status: "accepted"
---

# Context

ksync's semantic contract is "what production ArgoCD would apply" — and its performance target
(p50 ≤ 2s change→applied) leaves no room for spawning a kustomize process per render. ArgoCD
setups that consume the reference manifest shape run kustomize as
`kustomize build --enable-helm --load-restrictor LoadRestrictionsNone` (helm-inflating
kustomizations with a shared `chartHome` outside each kustomization root — a shape the default
root-only load restriction rejects, verified empirically).

# Decision

Render in-process with `sigs.k8s.io/kustomize/api` (krusty), replicating that exact CLI
invocation:

- `kustomize/api` is pinned at **v0.21.1** — the module behind the kustomize **5.8.1** binary,
  which is also the version argo-cd master bundles (`hack/tool-versions.sh`). The devShell
  installs the same binary version.
- Option mapping verified against the CLI source (`commands/build/build.go`):
  `Reorder = ReorderOptionUnspecified` (what the CLI passes when `--reorder` is not given),
  `LoadRestrictions = LoadRestrictionsNone`, `HelmConfig{Enabled: true, Command: "helm"}` over
  `MakeDefaultOptions()`. The helm `Command` is set explicitly because
  `types.EnabledPluginConfig` hardcodes a test-ism (`helmV3`).
- Helm stays an **external binary** (kustomize itself shells out to helm for `helmCharts`
  inflation; there is no in-process path). The devShell pins helm 4.2.0, matching argo-cd
  master's bundle.
- **Byte-parity is enforced by a test**, not assumed: `internal/render` compares in-process
  output against `kustomize build` output for the same fixtures (skipped when the binaries are
  not on PATH).

# Consequences

- No subprocess spawn per render; the renderer version is fixed by `go.mod`, immune to PATH
  drift across machines.
- M2's image-tag injection can run as an in-process `images` transformer on the rendered
  result instead of mutating overlay files on disk. It honors the kustomization's
  `configurations:` image field specs in addition to the builtin ones, so a dev tag reaches
  CRD-embedded image paths (e.g. a custom resource with a nested container image) exactly as
  `kustomize build` would for the same config — the reference manifests rely on this for CRDs
  whose image fields sit at non-standard paths.
- Any drift between the pinned module and the reference binary fails CI-visible tests
  immediately.

# Impact

- Bumping `kustomize/api` must track argo-cd's bundled kustomize version (and the devShell
  pin) — they move together or parity breaks.
- Helm version skew can still change chart output independently of kustomize; the devShell pin
  is the current mitigation. A `ksync render` vs `argocd app diff --local` validation recipe
  is future work.
- `LoadRestrictionsNone` means a kustomization can reference files anywhere on disk. For a
  local dev tool run by the manifest author this is the accepted trade-off; it is exactly what
  the reference ArgoCD configuration does.

# Alternatives

- **Shell out to the kustomize binary** — rejected: process spawn + YAML re-parse per render
  works against the latency target, the binary version varies by machine, and in-process
  transforms (M2) become impossible. The parity test keeps the in-process path honest, which
  was the main argument for shelling out.
- **Inflate charts via the helm Go SDK** — rejected: kustomize's `helmCharts` generator shells
  out to the helm binary; doing anything else risks output drift from the reference path.
- **Keep `LoadRestrictionsRootOnly`** — rejected: the reference manifest shape (shared
  `chartHome: ../../common`) cannot render under it.

# Notes

- Some ArgoCD setups additionally enable exec/alpha plugins (`--enable-alpha-plugins
  --enable-exec`) for secret generators in the ksops style. ksync does not enable plugin
  execution yet; apps whose kustomizations use exec generators fail to render until a config
  switch lands. Tracked as M1 follow-up work.
- The kustomization-level `namespace:` transformer does **not** touch helm-inflated objects;
  the reference shape namespaces chart output by passing `helmCharts[].namespace` into
  `helm template --namespace`, with chart templates stamping `.Release.Namespace`. The fixture
  mirrors this so nobody "fixes" it into divergence from the binary.
