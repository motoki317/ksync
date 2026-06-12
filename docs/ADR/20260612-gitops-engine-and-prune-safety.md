---
date: "2026-06-12"
author: "motoki317"
status: "accepted"
---

# Context

ksync promises ArgoCD-parity sync semantics (hooks, waves, prune, SSA, health) and a fast
change→applied loop. Building that from scratch is months of subtle work; consuming ArgoCD's
own sync engine gives the semantics by construction. Two safety questions need explicit
answers: how ksync decides what it owns (prune must never touch anything else), and how it
connects to a cluster (a kubeconfig usually also holds production contexts).

# Decision

- **Engine**: `github.com/argoproj/argo-cd/gitops-engine` (the monorepo path; the standalone
  repo was archived Sep 2025). The nested module is untagged, so it is pinned at the commit of
  an argo-cd release tag — currently **v3.4.3** → pseudo-version
  `v0.0.0-20260528113041-1801122b4391`. Upgrades follow argo-cd releases, never `@master`.
- **k8s.io/\* pinned to the engine's baseline** (v0.34.0). The engine's own go.mod replaces
  k8s.io modules at that version; newer ones even fail to compile against it (kubectl's
  scheme imports APIs that were removed since). ksync follows the engine's pin and moves only
  when it moves.
- **All engine usage lives in `internal/engine`** behind a small surface (`New`, `Sync`,
  `Close`), so the v0.x API churn the monorepo explicitly reserves the right to is contained
  in one package.
- **Ownership = tracking label** `ksync.dev/app: <app-name>`, stamped on copies of the
  rendered objects at sync time. The cluster cache memoizes the label value per resource, and
  the `isManaged` predicate — which is the *only* input to prune — matches resources whose
  value equals the app being synced. No label, or someone else's value → invisible to prune.
- **Server-side apply always**, with field manager `ksync` (production parity: the reference
  ArgoCD setup applies everything with `ServerSideApply=true`).
- **Connection requires a named context**: `RESTConfig(context)` errors on an empty name and
  never reads the kubeconfig's current-context.

# Consequences

- Hooks (including PostSync re-running every sync), waves, delete policies, health
  assessment, and prune ordering arrive engine-tested instead of hand-built.
- One long-running cluster cache with open watches makes repeat syncs diff against cached
  state — the performance core of the design.
- Verified live on first wiring: removing a release from the target pruned exactly the
  tracked resource and left unmanaged resources in the same namespace untouched.

# Impact

- The argo-cd pseudo-version pin is opaque to glance at; the release-tag mapping must be
  recorded here/in commit messages on every bump.
- k8s.io v0.34 means client-side feature gates newer than that are unavailable; acceptable
  because the engine is the only serious k8s API consumer.
- Label-based tracking puts ksync metadata on managed resources (visible in `--show-labels`);
  ArgoCD's annotation-based `trackingMethod` variant can be adopted later if label collisions
  ever materialize.

# Alternatives

- **Building on `fluxcd/pkg/ssa` or cli-utils** — rejected: no helm-hook semantics; the whole
  point is ArgoCD parity.
- **Shelling out to `argocd app sync --local`** — rejected: 5–30s per iteration, rejects
  auto-sync apps, requires a running ArgoCD.
- **`@master` engine builds** — rejected: argo-cd release tags are the only points known to
  hold the engine and its k8s pins consistent.
- **Annotation-based tracking (ArgoCD's `trackingMethod: annotation`)** — deferred: labels
  are simpler to inspect and select on, and the collision risk it mitigates involves apps
  also managed by other label-based tooling, which a local dev cluster does not have.

# Notes

- Namespace-less resources currently rely on the manifests carrying namespaces (the reference
  chart stamps `.Release.Namespace`). A `CreateNamespace`-equivalent and a default-namespace
  option are M1 follow-ups.
- `ksync destroy` will be `Sync` with an empty target set and prune enabled; `diff` comes from
  the engine's diff package against the warm cache. Both land with the watch loop wiring.
