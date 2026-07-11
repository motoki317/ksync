# ArgoCD parity: the semantic contract

ksync's semantic contract is "what production ArgoCD would apply." This is the verified reference
for that contract — the facts ksync replicates, each checked at source level against
`gitops-engine` (the argo-cd sync engine ksync consumes; see
[ADR 20260612-gitops-engine-and-prune-safety](ADR/20260612-gitops-engine-and-prune-safety.md)).
The user-facing summary is `ksync help hooks`; this document keeps the citations so the contract
can be re-verified rather than re-guessed.

Source paths below are within the argo-cd monorepo's `gitops-engine` tree (the standalone
`argoproj/gitops-engine` repo was archived Sep 2025; history migrated into argo-cd, where it is
maintained). Pin to argo-cd release tags — see the engine ADR.

## Hooks

- **Hook detection is source-type-independent.** The engine inspects annotations on rendered
  `unstructured.Unstructured` objects only — it does not care whether Helm, kustomize, or plain
  YAML produced them. `IsHook()` checks `argocd.argoproj.io/hook`, then falls back to
  `helm.sh/hook` (excluding `crd-install`).
  Source: `pkg/sync/hook/hook.go`, `pkg/sync/hook/helm/hook.go`.
- **Hook type mapping** (`pkg/sync/hook/helm/type.go`):
  - `pre-install` / `pre-upgrade` → **PreSync**
  - `post-install` / `post-upgrade` → **PostSync** (runs after main apply succeeds *and* resources
    are Healthy)
  - `crd-install` → normal resource
  - `test-*` / `*-rollback` → ignored
  - `pre/post-delete` are handled in argo-cd's controller (app deletion only) — out of ksync scope.
- **`helm.sh/hook-weight` is honored as the sync wave** when `argocd.argoproj.io/sync-wave` is
  absent. Source: `pkg/sync/syncwaves/waves.go`.
- **`helm.sh/hook-delete-policy` maps 1:1** (`before-hook-creation` / `hook-succeeded` /
  `hook-failed`); default is `BeforeHookCreation`, same as Helm.
- **Per-object precedence**: any `argocd.argoproj.io/hook` annotation on an object makes the
  `helm.sh/*` annotations on that same object ignored.
- **The behavioral difference that matters vs Helm/kluctl**: ArgoCD **re-runs `post-install`
  hooks on every sync** (PostSync phase), not just the first install. The reference environment
  relies on this — post-install Jobs with `before-hook-creation,hook-succeeded` delete policies
  re-run each sync. ksync follows ArgoCD semantics, not Helm install/upgrade semantics. See
  [ADR 20260616-hook-rerun-on-failure](ADR/20260616-hook-rerun-on-failure.md).

## Apply, ownership, prune

- **Server-side apply**: `WithServerSideApply` SyncOpt plus per-resource
  `argocd.argoproj.io/sync-options: ServerSideApply=true`. Source: `pkg/sync/sync_context.go`.
  The reference environment applies everything with `ServerSideApply=true`, so ksync defaults to
  SSA.
- **Prune requires knowing the managed set.** ArgoCD tracks ownership with a label/annotation
  (`app.kubernetes.io/instance` or `argocd.argoproj.io/tracking-id`). ksync stamps its own
  tracking label (`ksync.dev/app: <app>`) and scopes prune to it — prune must never touch a
  resource ksync does not own. Reverse-wave prune and `PruneLast` come from the engine
  (`pkg/sync`).

## The performance unlock

`gitops-engine/pkg/cache` keeps cluster watches open between syncs, so repeated syncs diff against
cached live state instead of re-listing the cluster — the exact cost that makes one-shot CLIs slow
in a watch loop. This is why ksync must be a **long-running process**, never spawn-per-change. See
[ADR 20260612-sync-performance](ADR/20260612-sync-performance.md).

## What the engine provides (so ksync does not build it)

`pkg/engine` (orchestration), `pkg/sync` (hooks, waves, prune incl. reverse-wave / `PruneLast`,
delete policies, SSA, sync options), `pkg/diff`, `pkg/health` (per-resource health assessment),
`pkg/cache` (live cluster-state cache with open watches). ksync wraps all of it behind
`internal/engine` to contain the v0.x API churn the monorepo reserves the right to.
