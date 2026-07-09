---
date: "2026-07-09"
author: "motoki317"
status: "accepted"
---

# Context

A one-shot `ksync sync` failed the whole run on the first apply error, with no retry at any
level. On a fresh single-node cluster this turned a self-healing race into a hard failure: an
app whose kustomization renders a chart that ships ~25 CRDs **and raw custom resources of those
kinds** raced its own CRD registration — the CRDs applied, but the immediately-following CRs
failed with `no matches for kind … ensure CRDs are installed first`. Consecutive CI runs failed
sub-second on exactly this, and one variant hung until the deadline with the CR reported
`Missing — not yet created` by the health gate. Because one app's failure cancels its siblings,
one racing app aborted a 23-app run at ~10s ("Apps 0 synced").

Server-side ordering was never the problem — gitops-engine already sorts CRDs before custom
kinds, barriers between kind groups, and polls each new CRD to Established. The blocker is
client-side REST-mapping staleness in the apply path. Live applies go through a kubectl factory
whose discovery is the on-disk cache (`~/.kube/cache/discovery`, 6h TTL). client-go's
`DeferredDiscoveryRESTMapper` self-heals a mapping miss **only when the discovery data did not
come from a live fetch** (`!cl.Fresh()`): on a fresh cluster the very first factory live-fetches
discovery — before the CRD is registered — so `Fresh()` is true and the CR's missing kind is
treated as final. A *new* factory, built the next iteration, reads the now-populated disk cache
(`Fresh()` false), so a miss triggers `Reset()`→`Invalidate()`→a live re-fetch that picks up the
just-registered kind. ksync builds a fresh factory (a fresh `SyncContext`) every loop iteration,
so **a second attempt resolves what the first could not** — but nothing retried, so that second
attempt never happened.

The same one-shot fail-fast masks every transient apply failure this way: an admission webhook
not yet serving, an apiserver hiccup, an SSA conflict, a PostSync hook that failed once. ArgoCD
converges through all of these by reconciling repeatedly; ksync did not.

# Decision

Make `engine.Sync` **converge until the app is applied-and-healthy or `--timeout` expires**,
retrying every failure, and make each failure, retry, and recovery visible in realtime on both a
terminal and off it.

- **Always retry, no failure classification (D1).** Any completed operation that ended in
  Error/Failed, or that carries a `SyncFailed` task result, is retried — never matched against
  the error text. This deletes a brittle message classifier before it is written and covers the
  CRD race, webhook-not-serving, apiserver flakes, SSA conflicts, and transient hook failures
  with one contract.
- **One code path (D2).** The former reconcile loop and post-apply health gate become a single
  labeled convergence loop. A warm cluster, where nothing fails, executes the same steps as
  before — the retry state is only ever touched in an already-failing branch — so the clean-path
  latency target (p50 ≤ 2s) is untouched.
- **Strip the failed results to re-attempt (D3).** gitops-engine seeds each task's state from the
  carried results; a task seeded as completed-and-unsuccessful short-circuits the whole next
  operation straight back to Failed **without re-running it**. So re-attempting a task means
  *removing its result from the carried set* before the next `WithInitialState`. ksync strips
  every result whose `HookPhase` is completed-but-unsuccessful (an apply failure carries
  `HookPhase` Error; a failed hook carries Failed while its `Status` stays Synced — both must be
  dropped) and keeps succeeded resources and completed hooks, so they are not re-run. The next
  iteration re-seeds `OperationRunning` with the stripped results and the *same* `startedAt` (so a
  hook's generated name is stable and BeforeHookCreation deletes-and-recreates it).
- **Backoff, timeout is the only bound (D4, D5).** Between attempts ksync waits `1s`, doubling to
  a `30s` cap, resetting to base when the failing-resource set strictly shrinks (progress), so a
  slowly-converging sync is not throttled like a wedged one. The wait is ctx-interruptible. There
  is no retry-limit knob and no `--fail-fast` flag: `--timeout` (default 5m) already bounds the
  run and is the natural failure boundary — fewer knobs than ArgoCD's limit/backoff policy.
- **Re-apply a persistently-Missing target (D6).** A target still `Missing` after a successful
  apply — a CR applied against a stale mapping that silently never got created — re-enters the
  apply path (its succeeded result is stripped by key to force it), paced so a resource merely not
  yet observed by the cache on the clean path is not re-applied, and backed off so a wedged one
  does not hot-loop. This closes the observed 10-minute silent-hang variant.
- **Re-fill default namespaces each convergence cycle (D7).** A namespaced CR with no explicit
  namespace takes the app's default namespace from a one-time `fillDefaultNamespace` at Sync start.
  That fill can only classify kinds the cluster already serves (`IsNamespaced`), so a CR whose CRD
  is still registering reads as cluster-scoped then and keeps an empty namespace — while the object
  the apply eventually creates lands under the app namespace. The health gate keys the target by
  namespace, so an empty-namespaced target never matches the live object and reads `Missing`
  forever. Without always-retry this was invisible: the sync failed fast and never reached a
  persistent health gate. Convergence exposed it — a sync that *had* applied-and-healthy resources
  timed out. The fix keeps the pre-loop fill (so `revision()` and every once-before-the-loop
  consumer still hash the filled target) and *adds* a re-fill at the top of each cycle: once the CRD
  is served, `IsNamespaced` returns true, the target key gains the real namespace, and the gate
  matches the live object. Idempotent (fills only an empty namespace), so the clean path is a no-op
  after the pre-loop fill.
- **Name the cause in the timeout (D8).** `TimeoutError` gains the retry count and the last apply
  failure, so a timeout after repeated failures reports *why* it never converged, not just which
  resource is still not healthy.
- **Distinct-event observability (D9).** A new UI-neutral `SyncOptions.OnRetry` fires on each
  failed attempt and once on recovery. The command layer renders it as: a transient live-row
  countdown on a terminal (`retrying after apply failure (attempt 2, next in 2s)`), and a
  **committed** line for each *distinct* failure and for recovery in both a terminal's scrollback
  and a CI log's stream (`⚠ shop  apply failed (attempt 1, retrying in 1s): <resource>: <error>`
  on a terminal; `shop deploy [2.1s] │ apply failed …` off it). Distinct failures are deduped by
  message, so a slow failure yields a few lines, not one per attempt; the committed deploy line
  gains a `(N retries)` suffix. The audience is a first-time user: every line names the resource
  and states what ksync is doing, with no engine jargon.
- **Watch keeps its scheduler-level retry visible too.** Engine convergence runs identically
  inside each watch sync, and the scheduler's whole-app retry — previously silent — now announces
  `shop: sync failed (attempt 2); retrying in 4s: <error>`.

# Consequences

- A fresh-cluster sync that races its own CRD registration (or waits on a webhook that is not yet
  serving) self-heals within seconds instead of failing the run; a `needs` chain no longer aborts
  because one dependency raced.
- A genuinely broken manifest under `ksync sync` now fails at `--timeout` (default 5m) rather than
  in ~2s. This is the deliberate cost of always-retry, mitigated by the failure being visible from
  attempt 1 (the first `⚠`/stream line names the exact error) and by Ctrl-C producing the usual
  diagnostics dump. `--timeout 0` ("no limit") consequently also removes the failure bound: a
  broken app retries until it converges or is interrupted. A wrapper that calls ksync as its deploy
  engine and imposes its own outer timeout shorter than `--timeout` will now cut ksync off
  mid-convergence where it previously got a fast typed error — set the two timeouts consistently.
- The warm-cluster clean path adds no reconciles, diffs, API calls, or output when nothing fails —
  the retry state is only touched in an already-failing branch. (Not literally byte-identical: the
  per-cycle namespace re-fill and a per-poll `hasMissing` check run, both in-memory and immaterial
  to the p50/p95 targets.)
- A `SyncFail`-phase hook (`argocd.argoproj.io/hook: SyncFail`) that *succeeds* runs about once for
  the whole convergence, not once per attempt: its Succeeded result is kept across retries, so
  gitops-engine treats it as done and does not re-run it. Only a SyncFail hook that itself *fails*
  is dropped by the strip and re-runs each attempt. Rare in kustomize apps; called out because the
  re-drive makes the exact semantics non-obvious.
- Render failures stay fail-fast (a render error is almost always the user's own manifest, where
  instant failure is the right dev-loop UX). `diff` is unchanged. `destroy` is unchanged too, but
  deliberately: it sets `SyncOptions.FailFast`, opting out of the retry so a wedged delete (an
  RBAC-forbidden or webhook-denied prune) surfaces at once instead of retrying for `--timeout` —
  the fail-fast semantics `destroy` documents. Without that opt-out `destroy` would silently inherit
  the retry.

# Impact

- `internal/engine/converge.go` (new): the retry helpers, all pure and unit-tested — result
  stripping, the backoff/progress state machine, the `missingReapply` poll-budget pacing, the
  `failedResult` predicate (the shared "is it failing" test — a SyncFailed status *or* a
  completed-unsuccessful HookPhase, so a failed PostSync hook, whose Status stays Synced, is
  reported and counted, not just an apply failure), `RetryEvent`, and the ctx-interruptible wait.
- `internal/engine/contract_test.go` (new): pins the three undocumented gitops-engine invariants
  the strip lever rests on (completed-unsuccessful → short-circuit; stripped → re-run;
  succeeded-kept → not re-run) through the exported `sync.NewSyncContext`/`Sync()` with a mock
  kubectl, so a future engine pin bump that changes the semantics fails CI instead of at runtime.
- `internal/engine/sync.go`: the two loops become one labeled convergence loop; the loop re-fills
  default namespaces each cycle (D7); `SyncOptions` gains `OnRetry` and `FailFast` (destroy's
  opt-out of the retry); `TimeoutError` gains `Retries`/`LastFailure`; `syncFailedError`/
  `failedResultsError` (which existed to surface a failure as a terminal error) are removed — a
  failure is now retried, not returned — while a small `failFastError` frames the FailFast return.
- `internal/ui/pipeline.go`: `Stage.Event` (a committed notice — `<symbol> <app>  <text>` on a
  terminal, stream-prefixed off it) and `Stage.SetTailQuiet` (a transient row update that never
  streams off a terminal, so the per-attempt countdown does not flood a CI log).
- `cmd/ksync/main.go`: `deployRetry` wires `OnRetry` at both sync sites (one-shot and watch),
  dedupes by message, and records the retry count for the `(N retries)` suffix.
- `internal/schedule/scheduler.go` + `internal/loop/loop.go`: `Scheduler.Finish` returns the new
  attempt count and backoff so the watch loop can announce a scheduled retry; `runDeploy` carries
  its failure cause out for that one line instead of logging inline.
- The gitops-engine pin is unchanged; no fork, no `replace`.

# Alternatives

- **Classify failures and retry only the retryable ones.** Rejected: it means matching error
  strings (`no matches for kind`, webhook messages) that the engine surfaces only as free text —
  brittle, and it would have to be extended for every new transient. Always-retry needs no
  classifier and cannot miss a case.
- **A staged two-phase apply: CRDs first with a discovery barrier, then the rest.** Rejected: a
  second apply code path to maintain, and it breaks PreSync-hook ordering parity with ArgoCD.
  Always-retry reuses the one path and lets the existing per-iteration factory do the healing.
- **An ArgoCD-style retry-limit / backoff policy knob.** Rejected: `--timeout` already bounds the
  run; a second bound is a knob without a job.
- **Force CRDs to Established as a health condition before applying dependents.** Rejected:
  unnecessary under always-retry (it would only shave one retry iteration off the race) and it
  does not help the non-CRD transient failures the same design already covers.

# Notes

The load-bearing claim — that stripping a failed result re-runs exactly that task — was verified
in the gitops-engine source (`pkg/sync/sync_context.go`): a task looks up its carried result by
`resultKey()`; with the result absent the task stays pending and is applied again, while the
`Fresh()`→`Reset()`→`Invalidate()` self-heal in client-go's `restmapper/discovery.go` is what
makes the re-applied CR resolve its now-registered kind. Because this rests on *undocumented*
engine internals — a version bump could break it silently, with the worst case a false-success on
a broken deploy — `contract_test.go` now pins the three invariants (short-circuit, strip→re-run,
succeeded-kept→not-re-run) through the exported API with a mock kubectl, so such a bump fails CI.

Validated live against a real cluster (output redirected to a file, so the non-terminal stream
path), an app rendering a namespaced `Route` CR whose CRD was absent:

- The first pass proved retry and recovery but *not* convergence, and it is what surfaced D7.
  Apply failed and retried with backoff (`apply failed (attempt 1, retrying in 1s): …failed to
  discover server resources…`); the CRD was applied out-of-band mid-retry; the next attempt's
  fresh factory re-resolved the kind and the apply succeeded (`apply recovered after N retries`).
  But the run then **timed out on the health gate**, reporting the just-created Route as
  `Route//main: Missing` — an empty namespace. The reconcile logs showed the cache did hold the
  object (`Route:team-a/main obj->obj` every cycle); the gate was keying an empty-namespaced target
  because the one-time namespace fill ran before the CRD registered. The cache was never blind —
  the target key was wrong. That is D7.
- With the per-cycle re-fill, the same race **converges to exit 0**: `apply recovered after 4
  retries` → the health gate matches the live Route → `✓ 🚢 Deploy shop  2 applied  (4 retries)`,
  process exit 0 in ~17s. A control run with the CRD registered *before* start exits 0 in ~2s,
  isolating the gap to the post-registration path and confirming the fix closes exactly it.
- A separate run left the CRD absent to the deadline and confirmed the enriched timeout: `sync of
  "shop" timed out after N retries; last failure: …failed to discover server resources…; still not
  healthy: …Route//main: Missing`. (The `Missing` line keeps the empty namespace here because
  `IsNamespaced` never flips without the CRD — cosmetic; the `last failure` line already names the
  real cause.)

Builds on the health-gate contract (ADR 20260614-sync-health-gate), the section-spacing contract
(ADR 20260619-section-output-spacing), and the non-terminal streaming model (ADR
20260701-non-terminal-log-streaming) — every committed retry line is a `SectionPipeline` write,
so the console spaces it automatically, and it carries no heartbeat.
