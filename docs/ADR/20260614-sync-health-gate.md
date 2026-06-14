---
date: "2026-06-14"
author: "motoki317"
status: "accepted"
---

# Context

A ksync `Sync` should mean the app is *deployed and running*, not merely *applied*. Two features
depend on that meaning:

- **`needs` ordering.** ksync starts a dependent app only after the apps it needs have finished
  syncing. If "finished" means "manifests applied", a dependent races its dependency's pods to
  Ready — e.g. an app starts against a database whose StatefulSet is still pulling its image. The
  `needs` edge then orders *apply*, not *availability*, which is not what it is for.
- **Operator feedback.** A whole-stack sync that reports `✓ N applied` in 0.7s, while half the
  workloads are still `ContainerCreating`, hides whether the deploy actually came up.

gitops-engine does **not** wait for health on the common path. `syncContext.Sync()` health-gates
*between* sync waves, but for the last wave (or an app with no waves/hooks at all — every plain
chart) it sets `OperationSucceeded` the moment the resources are *applied*:

```go
// sync_context.go
case successful:
    if remainingTasks.Len() == 0 && !tasks.Any(isHook) {
        // ... we are successful EVEN if those objects subsequently degrade.
        // This handles the common case where neither hooks or waves are used and
        // a sync equates to simply an (asynchronous) kubectl apply ...
        sc.setOperationPhase(common.OperationSucceeded, "successfully synced (all tasks run)")
```

So ksync's re-reconciling loop returns as soon as a single-wave app is applied. Health gating only
ever happened for multi-wave/hook apps, by accident of gitops-engine's wave logic — exactly the
apps that already block. The base infra (single-wave charts) never waited.

# Decision

After the apply loop reports success, `engine.Sync` runs a **health gate**: poll until every
non-hook target resource is observed `Healthy`/`Suspended`, or the sync deadline (`--timeout`)
fires and the error names what is still pending. The gate is `pendingHealth`/`pendingLines`:

- **Keyed on the target set, not the live set.** A resource the warm cache has not yet observed
  (just applied, watch event pending) counts as pending (`"not yet created"`), so the gate cannot
  declare a premature success during the window between apply and the first watch event.
- **Hooks excluded** (`hook.IsHook`): a hook Job with a delete policy is removed once it runs, so
  it is legitimately absent and must never hold the gate open. Hooks and health-gated waves were
  already awaited inside the apply loop.
- **No-health kinds are ready once present** (ConfigMap, Service, CRD, custom resources): they have
  no health check, so existence is convergence.

`SyncOptions.OnWait(pending)` is invoked once per poll while the gate waits, driving a live
"waiting for health" line in the CLI; an already-healthy sync returns without ever calling it.

# Consequences

- A `needs` edge now means "the dependency is serving", because `runByNeeds` releases a dependent
  only after the dependency's `Sync` returns, and `Sync` now returns only when healthy. Validated
  live: wiping `redis` and re-syncing blocked ~6s until its StatefulSet was `1/1`, and a 7-app
  infra sync had `needs`-gated apps complete after the apps they need.
- A whole-stack sync's elapsed time and live `k/N synced` footer reflect real convergence, not
  apply latency. Genuinely broken workloads are now caught by the existing timeout diagnostic
  (which names the still-unhealthy resources) instead of passing as `✓ applied`.
- An already-healthy (no-op) re-sync still returns immediately: the gate's first poll finds nothing
  pending and returns, so the warm fast path is unchanged.

# Impact

- A sync of an app that **cannot** become healthy now blocks until `--timeout` (default 5m) and
  then fails, where before it returned `✓ applied`. This is the point — it surfaces a real problem
  — but it means an app with an unmet *external* prerequisite (e.g. a database role or bucket a
  separate bootstrap provisions) will time out rather than silently "succeed". The error names the
  unhealthy resources so the cause is visible; the operator excludes that app or runs its bootstrap.
- The pure gate logic (`pendingLines`: target-keyed, hook-excluded, presence-required) is
  unit-tested; the end-to-end wait is covered by live-cluster reproduction (the engine package has
  no apiserver harness).

# Alternatives

- **Gate health in the scheduler/loop instead of the engine.** Rejected: health assessment needs
  the warm cluster cache and the tracking-label `isManaged` predicate, both of which live in
  `engine`; doing it there keeps `Sync` honest for every caller (one-shot, watch, destroy) with no
  duplicated health logic.
- **Walk the live set (like `AppDegraded`/`unhealthyLines`) instead of the target set.** Rejected:
  the live set omits a just-applied resource the cache has not yet seen, so the gate would return
  success in the apply→watch window. `AppDegraded` can tolerate that (it is a post-hoc snapshot that
  deliberately ignores Progressing/Missing); a *gate* cannot.
- **Fail fast on `Degraded` instead of waiting for the deadline.** Considered; deferred. A rollout
  is `Progressing`, not `Degraded`, until its progress deadline, so fast-failing risks little — but
  waiting-then-timeout keeps one clear failure path and one diagnostic, and lets a transiently
  degraded resource recover. Revisit if 5-minute waits on doomed rollouts prove annoying.

# Notes

`AppDegraded` is retained and unchanged: it is the *post-sync* health snapshot that flags a
`Degraded` resource as `⚠ … degraded` on an otherwise-successful sync (e.g. a no-op re-sync of an
app whose workload later broke). The gate (waits for Healthy) and the snapshot (flags Degraded
without blocking) are complementary, not redundant.
