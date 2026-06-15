---
date: "2026-06-16"
author: "motoki317"
status: "accepted"
---

# Context

`engine.Sync` decides `skipHooks` once, from the first reconcile, as `!diffModified` — a no-change
sync skips hooks, reproducing gitops-engine's own rule (see
20260614-sync-loop-hook-convergence). That is correct for the common case, but it strands a
**failed hook**: a PostSync Job's own source stops diffing the moment it exists, so once it has run
the app no longer reports a diff — and a hook that *failed* is therefore never retried by a plain
re-sync.

Dogfounding the full multi-service stack hit this precisely. A PostSync hook that provisions OAuth
clients and writes their secrets failed on a transient upstream 502 and exhausted its backoff
limit. Re-running `ksync sync` reported `✓ 0 applied` for that app — the hook was skipped (no diff,
and gitops-engine excludes hooks from the post-apply health gate, since a hook with a delete policy
is legitimately absent) — while the dependent app crash-looped forever on the missing secret. The
`needs` edge was meaningless: the dependency had "synced", but its hook had never succeeded, so the
secret it was supposed to produce did not exist. Worse, there was no way short of editing manifests
to force the hook to run again.

# Decision

Add two overrides to the "no diff → skip hooks" rule, so a hook that needs to run again does:

```go
skipHooks = !opts.Force && !diffRes.Modified && !hasDegradedHook(target, live)
```

1. **A currently-Degraded hook re-runs.** `hasDegradedHook` checks the live health of every target
   hook (keyed against the same `GetManagedLiveObjs` map the diff already uses, so no extra API
   calls). A hook whose Job failed or hit its backoff limit is `Degraded`, so the operation re-runs
   hooks even with an empty diff: its `BeforeHookCreation` delete policy removes the stale Job and
   recreates it, retrying the failure. The retry either succeeds — and the `needs` edge is finally
   satisfied — or fails the operation again (`OperationFailed`), which blocks dependents and exits
   non-zero, the honest outcome. A **succeeded** hook is `Healthy`, not `Degraded`, so it still
   diffs quiet and is not re-run: the idempotent no-op fast path is unchanged.

2. **`--force` re-runs every hook unconditionally** — ArgoCD's manual-sync semantics. This covers
   re-applying a release whose source did not change, and the case override (1) cannot see: a hook
   that failed and whose Job was already **deleted** (a TTL, a prior `BeforeHookCreation`) is
   *absent*, not `Degraded`, so only an explicit force re-runs it. `ksync sync --force` is the
   recovery command when a stack's hook side-effects went missing.

# Consequences

- A transiently-failed hook self-heals on the next `ksync sync` while its Job is still present, and
  `ksync sync --force` recovers it once the Job is gone. The dogfooded stack — a transient-502
  failure that left a dependent crash-looping on a missing secret — converges to `21 synced` once
  the hook is re-run.
- The fast path is untouched: a healthy stack's hooks are `Healthy`/absent, so `hasDegradedHook` is
  false and `--force` is unset, giving the same no-op `0 applied` as before.
- `--force` re-runs *all* hooks for the synced apps, including idempotent ones that did not need it;
  this is the intended manual-sync semantics, not a precise targeted retry.

# Impact

- `internal/engine`: `SyncOptions.Force`; `hasDegradedHook` (pure, unit-tested via `TestHasDegradedHook`);
  the `skipHooks` decision gains the two overrides.
- `cmd/ksync`: `sync` registers `--force`, threaded through `syncOneApp` into `SyncOptions`.

# Alternatives

- **Always re-run PostSync hooks** (literal "post-install/post-upgrade re-run on every sync").
  Rejected: it re-runs every provisioning Job on every no-op sync — slow across a whole stack, and
  it can churn generated secrets (each run rewriting credentials the running pods hold), forcing
  needless restarts. Re-running only a *failed* hook (or on explicit `--force`) gets the recovery
  without the churn.
- **Persist which hooks have succeeded.** Rejected: ksync deliberately holds no persisted sync/build
  state (startup re-derives everything); a success ledger would be the first exception and a new
  source of staleness. Live health is the source of truth already in the warm cache.
- **Make `AppDegraded` fatal** (a degraded post-sync snapshot fails the sync). Rejected as the
  primary fix: it conflates "broken hook that should retry" with "workload mid-rollout", and the
  health gate already blocks on degraded *non*-hook resources by waiting them out. Re-running the
  hook addresses the actual gap (hooks are gate-excluded) at its source.
