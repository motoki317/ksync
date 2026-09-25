# ArgoCD parity: the semantic contract

ksync must apply what production ArgoCD applies for the same manifests. The reference environment
is the production ArgoCD setup that ksync was built to match. Each behavior below is checked against
the source of gitops-engine, the ArgoCD sync engine that ksync uses
([ADR 20260612-gitops-engine-and-prune-safety](ADR/20260612-gitops-engine-and-prune-safety.md)).
The user-facing summary is `ksync help hooks`.

Source paths are relative to the `gitops-engine` tree of the argo-cd monorepo, at the commit pinned
in `go.mod` (argo-cd v3.4.3).

## Hooks

- **Hook detection does not depend on the source.** The engine reads annotations on the rendered
  `unstructured.Unstructured` objects. It does not care whether Helm, kustomize, or plain YAML
  produced them. `IsHook()` checks `argocd.argoproj.io/hook`. When that annotation is absent, it
  falls back to `helm.sh/hook`, except the value `crd-install`. Source: `pkg/sync/hook/hook.go`,
  `pkg/sync/hook/helm/hook.go`.
- **Hook type mapping** (`pkg/sync/hook/helm/type.go`):
  - `pre-install`, `pre-upgrade` → **PreSync**
  - `post-install`, `post-upgrade` → **PostSync**. It runs after the main apply succeeds and the
    resources are Healthy.
  - `crd-install` → a normal resource
  - any other value (`test`, `pre-rollback`, `post-rollback`) → ignored. The object counts as a
    hook but gets no sync phase, so the engine never applies it. Source: `pkg/sync/sync_phase.go`.
- **Delete hooks are out of scope.** argo-cd's controller, not the engine, handles `pre-delete` and
  `post-delete`, on app deletion only. Source: argo-cd `controller/hook.go`.
- **When `argocd.argoproj.io/sync-wave` is absent, `helm.sh/hook-weight` is the sync wave**, even on
  an object with an ArgoCD hook annotation. Source: `pkg/sync/syncwaves/waves.go`.
- **`helm.sh/hook-delete-policy` maps one-to-one** (`before-hook-creation`, `hook-succeeded`,
  `hook-failed`). The engine adds these to any `argocd.argoproj.io/hook-delete-policy` values on
  the same object. With no policy, the default is `BeforeHookCreation`, as in Helm. Source:
  `pkg/sync/hook/delete_policy.go`, `pkg/sync/hook/helm/delete_policy.go`.
- **The difference from Helm that matters:** ArgoCD runs `post-install` hooks again on every sync
  (PostSync phase), not only on the first install. The reference environment depends on this: its
  post-install Jobs use `before-hook-creation,hook-succeeded` delete policies and run on each sync.
- **A sync with no diff skips hooks**, as gitops-engine's own `Sync` does (`pkg/engine/engine.go`:
  `WithSkipHooks(!diffRes.Modified)`). A Degraded hook still runs again, and `--force` runs every
  hook, like an ArgoCD manual sync. See
  [ADR 20260616-hook-rerun-on-failure](ADR/20260616-hook-rerun-on-failure.md).

## Apply, ownership, prune

- **Server-side apply is an intended divergence.** Plain ArgoCD applies client-side unless an app
  sets the `ServerSideApply=true` sync option (argo-cd `controller/sync.go`). The engine
  applies server-side when the `WithServerSideApply` SyncOpt or the per-resource
  `argocd.argoproj.io/sync-options: ServerSideApply=true` annotation is set
  (`pkg/sync/sync_context.go`). The reference environment applies everything server-side, so ksync
  always sets the SyncOpt. The `ServerSideApply=true` annotation therefore changes nothing in
  ksync. The engine checks `ServerSideApply=false` first, so that annotation still applies one
  resource client-side.
- **Prune needs the managed set.** ArgoCD tracks ownership with the `app.kubernetes.io/instance`
  label or the `argocd.argoproj.io/tracking-id` annotation. ksync stamps its own tracking label,
  `ksync.dev/app: <app>`, and prunes only resources that carry it.

## What the engine provides (so ksync does not build it)

- `pkg/engine`: orchestration
- `pkg/sync`: hooks, waves, prune (reverse-wave order, `PruneLast`), delete policies, server-side
  apply, sync options
- `pkg/diff`: diffing
- `pkg/health`: health per resource
- `pkg/cache`: the live cluster-state cache with open watches
