---
date: "2026-06-14"
author: "motoki317"
status: "accepted"
---

# Context

ksync syncs each app through gitops-engine. The wrapper called `engine.GitOpsEngine.Sync` — the
convenience entry point in `pkg/engine` — to run one operation to completion: it reconciles the
target against live state, builds a `syncContext`, then loops `syncContext.Sync()` until the
operation phase is terminal.

Dogfooding the **full** multi-service stack we test against (not a single-app bench) surfaced a
hard failure: the first sync of an app whose chart ships a migration Job never converged. Every
workload reached Running, the Job (a Helm `pre-install,pre-upgrade` hook, re-tagged to an ArgoCD
`Sync` hook so it applies in-wave with the Secret it reads) reached Complete in ~3s — yet the
operation hung until the 5-minute timeout, which then reported *nothing* unhealthy. A second sync
converged instantly, because with no diff gitops-engine skips hooks entirely.

Root cause, confirmed by reproducing with gitops-engine's own V(1) logs: `GitOpsEngine.Sync`
reconciles live state **once** and reuses that snapshot (`sc.resources`) for the whole operation.
A hook resource is *created during* the sync, so it is absent from the snapshot; the syncContext
resolves a hook task's live object only by looking it up in that fixed snapshot
(`getSyncTasks` → `sc.liveObj`). The hook Job's live object therefore stays `nil` forever, its
health is never read, the hook task is never advanced past `Running`, and the operation waits on
`completion of hook batch/Job/<name>` indefinitely. The snapshot is never refreshed because
`sc.resources` is assigned only at `NewSyncContext` and the cache-update subscription in
`GitOpsEngine.Sync` only wakes for resources that were in the *initial* managed set — which a
freshly created hook is not.

This is not a niche case: it is the first install of **any** app that contains a creation hook —
exactly the common Helm-chart shape (migrations, CRD installs, bootstrap Jobs).

# Decision

Stop using `engine.GitOpsEngine.Sync` and drive `pkg/sync` directly in a **re-reconciling loop**,
which is what ArgoCD's application controller does. Each iteration:

1. `GetManagedLiveObjs` + `sync.Reconcile` against **fresh** live state — so a hook created in a
   previous iteration is now present and its live health can be read;
2. a new `syncContext` seeded with `sync.WithInitialState(phase, message, results, startedAt)`,
   threading the operation's accumulated results forward so completed work (and the hook itself)
   is recognized, not re-run;
3. one `syncContext.Sync()` step, then read `GetState()`; on a terminal phase return, otherwise
   wait `operationRefresh` (1s) and reconcile again.

`WithSkipHooks` is decided **once**, from the first reconcile (`!diffModified`), then held for the
whole operation. This reproduces `GitOpsEngine.Sync`'s rule exactly — a no-change sync skips hooks,
so its behavior is unchanged — but a *changed* sync keeps hooks enabled across every re-reconcile.
Holding it is what makes the wait correct: once a hook is created its per-resource diff goes quiet,
so re-deciding `skipHooks` per iteration would drop the still-running hook on the next pass and
report success early. Combined with `WithInitialState` (which makes a completed hook recognized,
not re-run) the hook runs once and is waited on to completion.

`Engine` now stores the `*rest.Config` and a `kube.Kubectl` (the same default
`engine.NewEngine` builds) so it can construct the syncContext itself. The cache lifecycle is
unchanged — `NewEngine(...).Run()` still warms the cache and returns its `Invalidate` stop.

# Consequences

- First install of an app with a creation hook converges as soon as the hook completes, instead of
  hanging to the timeout. Validated on a live k3d stack: the app with the migration-Job hook now
  reports `synced` once that Job reaches Complete.
- A hookless app still completes on the **first** iteration with no polling, so the common fast
  path is unchanged; only apps that genuinely wait (hooks, health-gated waves) poll at 1s.
- The timeout diagnostic ("which managed resource is still unhealthy") is retained and now fires
  only for genuinely stuck resources, not for this false hang.

# Impact

- The sync orchestration is ksync's own ~40 lines rather than a single library call, widening the
  gitops-engine surface ksync depends on (`pkg/sync.Reconcile`, `NewSyncContext`, the `SyncOpt`s,
  `pkg/utils/kube`/`tracing`). This is localized to `internal/engine`, the package whose explicit
  job is to absorb gitops-engine's v0.x churn.
- No behavioral delta versus the old wrapper for the hook/skip decision: `skipHooks` is still
  `!diffModified` from the first reconcile, just held constant for the operation. A no-change sync
  skips hooks as before; a changed sync runs them (and a hooked app re-runs its hook on every
  changed sync — inherent to a pre-upgrade/Sync hook, and the same as the project's own
  `helm`-based apply).
- No unit test exercises the loop end-to-end (the engine package has no apiserver harness); the
  fix is covered by live-cluster reproduction. `syncFailedError` (the new terminal-phase error
  path) is unit-tested.

# Alternatives

- **Keep `GitOpsEngine.Sync`, raise the timeout / treat "timed out but healthy" as success.**
  Rejected: it still blocks the loop for the whole timeout on every hook install and papers over a
  real convergence bug; dependent apps gated on the app would stall.
- **Reuse one long-lived syncContext and refresh its snapshot.** Not possible through the public
  `SyncContext` interface (`sc.resources` is unexported and never reassigned).
- **Bump gitops-engine hoping the wrapper re-reconciles.** Uncertain and out of step with pinning
  argo-cd release tags; the controller-style loop is the supported pattern regardless of version.

# Notes

The reproduction recipe (useful if this regresses): delete a hook Job and one normal resource of
the app to force a non-empty diff, then sync — the operation will wait on
`completion of hook .../Job/...` with the hook's task shown as `nil->obj` (live object never
resolved). With the fix the same scenario converges once the recreated hook completes.
