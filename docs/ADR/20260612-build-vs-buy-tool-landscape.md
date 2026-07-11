---
date: "2026-06-12"
author: "motoki317"
status: "accepted"
---

# Context

Before writing any code, one question had to be answered with evidence: does a tool already
cover ksync's target — a **watch loop** over local kustomize directories that renders, diffs,
and applies with **ArgoCD-parity semantics** (helm hooks, sync waves, prune, server-side apply,
health gating) and a **fast change→applied loop** (p50 ≤ 2s, p95 ≤ 5s single-app edit)? If one
did, ksync should not exist.

A 2026-06 landscape survey (deep multi-source research with adversarial verification, followed by
code-level fact checks against each candidate's source) concluded that **no existing tool covers
watch + helm hooks + ordering + auto-retry + high performance over local kustomize dirs**. The
decision recorded here is build-vs-buy: what to reuse, what to build, and why the survey's
conclusion stands until new evidence overturns it.

The **reference target shape** the survey measured against — the manifest shape ksync must
handle — is a production environment that deploys kustomize-rendered manifests carrying
`helm.sh/hook` annotations via ArgoCD:

- The large majority of its kustomization directories use `helmCharts` inflation (`kustomize
  build --enable-helm`) with a shared local `chartHome` and **multiple chart releases per
  kustomization** (commonly one shared chart inflated several times via YAML anchors).
- Helm hooks in use are `post-install` Jobs (`helm.sh/hook-weight: "0"`,
  `helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded`).
- **No `sync-wave` annotations anywhere** (grep-verified): cross-app ordering is *not* enforced
  by production ArgoCD — ApplicationSet apps sync independently. So the only ordering ksync owes
  is hook-weight *within* an app (free from the engine) plus an explicit app-dependency model
  ksync adds itself (`needs`).
- ArgoCD sync options in use globally: `ServerSideApply=true`, `CreateNamespace=true`.

# Decision

**Build ksync on `gitops-engine`, not on any existing end-to-end tool.** The survey's per-tool
disqualifiers (each verified against source, not marketing):

| Tool | Disqualifier (verified) |
|---|---|
| **Tilt** | No maintained helmfile/kustomize-hook story; image injection by name-match with randomized tags; the watch loop targets image rebuilds, not manifest sync semantics. |
| **Skaffold** | Custom builder mandates registry-push or a docker daemon with forced digest resolution ([#4624](https://github.com/GoogleContainerTools/skaffold/issues/4624), open since 2020). Parallel helm deploys exist, but no helm-hook emulation on kustomize-rendered output. |
| **kapp (Carvel)** | One-shot only, no watch. **Ignores `helm.sh/hook*` entirely** (zero code references). Documented perf issues re-listing cluster resources per run ([#366](https://github.com/carvel-dev/kapp/issues/366)). Feature-frozen post-Broadcom. |
| **kluctl** | Best off-the-shelf semantics (maps `helm.sh/hook` incl. weight + delete-policy, barriers, apply retries). BUT no watch mode (GitOps controller is git/OCI only), and `post-install` maps to *initial-deploy-only* (Helm semantics), whereas ArgoCD re-runs it every sync as PostSync — which the reference environment relies on. Useful only as a benchmark baseline. |
| **kpt live / cli-utils** | Inventory + prune, but no text diff, no hooks; library near-dormant. |
| **kubectl ApplySet (KEP-3659)** | The native future answer; alpha since v1.27 (2023), still env-gated and stalled. Watch it; don't depend on it. |
| **`argocd app sync --local`** | Production-parity semantics, zero code — but renders client-side, **hard-rejects apps with auto-sync enabled**, no multi-source apps, ~5–30s/iteration (+ hardcoded 2s/sync-wave delay). Fails the perf bar; kept only as an occasional validation tool. |
| **Flux `fluxcd/pkg/ssa`** | Solid SSA apply/diff/wait/prune, **no helm hook support** (in Flux only helm-controller runs hooks, via real Helm). |

**The decisive enabler**: ArgoCD's sync engine — the exact code that gives production its
semantics — is a consumable Go library, `gitops-engine`. Its `agent/` example is a ~240-line
`main.go` wiring `ClusterCache` + `GitOpsEngine.Sync` into a directory-to-cluster syncer. ksync
is structurally that example plus an fsnotify watcher, per-app incremental rendering, and a TUI.
The engine choice, its pinning, and prune-safety are recorded in
[20260612-gitops-engine-and-prune-safety](20260612-gitops-engine-and-prune-safety.md); the
verified hook/wave/SSA facts ksync must replicate are in [../argocd-parity.md](../argocd-parity.md).

# Consequences

- **ksync is a Go project** — gitops-engine is Go, and kustomize is consumable in-process via
  `sigs.k8s.io/kustomize/api` (krusty). This is settled, not re-open per feature.
- **The novel code is bounded**: watcher + dirty-set, incremental render orchestration, app
  dependency scheduling, TUI, and build/loader plumbing. Watch mode, hooks, ordering, retry, SSA,
  prune, and health assessment come from the engine — do not re-implement them.
- **Long-running process, warm cluster cache** is mandatory: `gitops-engine/pkg/cache` keeps
  cluster watches open between syncs so repeat syncs diff against cached state. Spawn-per-change
  forfeits the entire performance advantage. See
  [20260612-sync-performance](20260612-sync-performance.md).

# Impact

- ksync is scoped as a **development tool**, explicitly NOT: a production release/CI tool (CI
  builds/pushes images, ArgoCD deploys prod); a hot-reload/file-sync-into-container tool
  (mirrord/Telepresence own that inner loop); a GitOps controller (no in-cluster component;
  push-based, local-first). Requests that pull toward those niches are out of scope by decision.
- The survey is a point-in-time result. **Re-litigate tool selection only with new evidence** — a
  candidate shipping the missing capability (e.g. ApplySet reaching stable with hook parity), not
  a fresh preference.

# Alternatives

Every row of the table above is a rejected alternative. The two closest calls: **kluctl** (right
semantics, no watch, wrong `post-install` timing) and **ApplySet** (native future, not yet
usable). Both are worth re-checking when their gaps close.

# Notes

- **Naming caveat**: an unrelated, abandoned project `vapor-ware/ksync` (file-sync into pods)
  exists. Different niche, dead project — not a functional collision, but worth knowing when
  publishing.
- This ADR was extracted on 2026-07-11 from a previously untracked local handoff document, so the
  research context stops depending on a machine-local file. Private references in the original
  (specific cluster/repo names) are deliberately omitted; only the de-identified shape survives.
