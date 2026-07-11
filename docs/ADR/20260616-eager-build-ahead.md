---
date: "2026-06-16"
author: "motoki317"
status: "accepted"
---

# Context

A watch-mode run of one app is build → import → render → sync, driven by `internal/schedule`. The
scheduler gates the *whole* run on the app's `needs` edges: `StartDue` will not start an app until
every dependency is clean (deployed and Healthy, since the health gate holds the dependency's run
open until then — see 20260614-sync-health-gate). That gating is correct for the *deploy*: a
dependent must not sync against a half-updated dependency, and `needs` exists to order deploys.

But it also gates the **build and image-import**, which are dependency-independent. Building an
image is a pure-local docker build; importing it (`imageLoad`) loads it into the cluster's image
store. Neither reads anything about a dependency's *deployed* state. Yet a chain `db → auth → api-b`,
each with a docker build, builds strictly one app at a time, each build waiting behind the previous
app's full deploy **and** its health gate. On a cold convergence — many apps, a deep `needs` DAG,
a build on most links — the builds, which are the bulk of the wall-clock, run fully serialized
behind the deploy chain instead of overlapping it.

This affects **both** entry points, which share the build-then-deploy-fused shape: the watch loop
(`internal/loop`, build inside the scheduled run) and the one-shot `sync` (`cmd/ksync`, build inside
`syncOneApp` run under `runByNeeds`). The one-shot path matters especially — it is the cold-stack
convergence command — and the user-facing guide already promised "a one-time sync is never slower
than the loop's startup pass," which fixing only the loop would have broken.

While tracing this, a second issue surfaced: `Scheduler.NextDeadline` reported a dirty app's
deadline even when the app was `needs`-blocked. A blocked dependent's debounce deadline is already
in the past while its dependency runs (seconds–minutes in the health gate), so the loop's
`time.After` fired immediately every iteration and spun until the dependency's `Finish`.

# Decision

Split the run into two phases scheduled independently, by running **two** `Scheduler` instances —
reusing the existing, tested state machine rather than rewriting it:

- **`buildSched`** holds the build-apps with **no `needs` edges**. A source change marks the app
  dirty here, so its image builds the moment its source changes, regardless of the DAG — builds
  across the whole stack overlap each other and the deploy chain.
- **`deploySched`** holds all apps **with their `needs` edges** — the existing deploy gating. It
  also gets one **external gate** (`Scheduler.SetExternalBlock`): a deploy is held until the app's
  own image has finished building, so an unbuilt content-addressed tag is never deployed. The gate
  is "this app has a build in flight or a still-dirty build entry," supplied by the loop from its
  build bookkeeping.

Every change marks `deploySched` dirty (the app must re-deploy); a *source* change additionally
marks `buildSched` and the build entry. The deploy's external gate makes it wait when a rebuild is
also pending, so a manifest+source edit deploys once, with the fresh tag. A build failure re-dirties
the entry — `buildSched` retries with backoff while the deploy stays gated, preserving "never deploy
an unbuilt image."

The external gate also closes the `NextDeadline` spin: a blocked app's unblocking is an *event* (a
dependency's `Finish`, or a build result re-polling `StartDue`), never the clock, so `NextDeadline`
now skips both `needs`-blocked and externally-blocked apps — the same reasoning that already
excludes running apps.

# Consequences

- On a cold convergence, a dependent's image builds while its dependency is still deploying; by the
  time the dependency goes Healthy the dependent only needs the sync step. The serialized
  build-behind-deploy chain becomes build-overlapped-with-deploy.
- `--max-parallel N` now bounds the build phase and the deploy phase with **independent** budgets of
  `N`, where before it bounded the single fused run. Peak concurrency can reach `2N` goroutines, but
  only up to `N` are the CPU/IO-heavy builds; deploys are mostly health-gate waiting, cheap to
  overlap. This separation is what lets builds keep running while deploys wait.
- The `NextDeadline` fix removes a busy-loop that already existed for `needs`-blocked apps and that
  eager builds would have made more frequent.
- Every prior invariant holds: per-app deploy serialization, `needs` ordering on deploys, retry
  backoff per phase, and "never deploy an unbuilt image." The `imageLoad` Loader still serializes
  its calls internally (k3d image import is not concurrency-safe; _20260615-imageload-concurrency),
  so more concurrent builds remain safe.

# Impact

- `internal/schedule`: `Scheduler.SetExternalBlock` (optional, nil by default); `StartDue` and
  `NextDeadline` consult it; `NextDeadline` additionally skips `needs`-blocked apps. Unit-tested
  (`TestScheduler_ExternalBlockGatesStartUntilCleared`, `TestScheduler_NextDeadlineSkipsBlockedApps`).
- `internal/loop` (watch): two schedulers; `building` map + `buildPending` form the deploy's
  external gate; `runner.run` splits into the build goroutine (`BuildAll`) and `runner.runDeploy`;
  two result streams; `earliestDeadline` merges the two timers.
  `TestRun_DependentBuildsWhileDependencyDeploys` proves a dependent builds while its dependency's
  deploy is held in flight (the old fused run would deadlock there).
- `cmd/ksync` (one-shot `sync`): `startEagerBuilds` launches the build goroutines up front
  (needs-free, bounded), returning an `await` the deploy phase calls; `syncOneApp` becomes the
  build-less `deployApp`; `runByNeeds` is unchanged (it now gates only the deploy, which awaits its
  own build via `await`). `TestStartEagerBuilds_*` cover the concurrency and error propagation.

# Alternatives

- **Extend the single scheduler to a native two-phase model.** Rejected: a larger rewrite of the
  pure, heavily-tested state machine for no extra capability. Two instances reuse the whole machine;
  the only new scheduler code is an optional external gate.
- **Eager-build pool outside the scheduler, deploys unchanged.** Rejected: it would re-implement
  debounce, parallelism, and retry-backoff for builds — exactly what the scheduler already provides
  — and fragment scheduling logic across two places.
- **Mark the deploy dirty only on build completion** (instead of an external gate). Rejected: a
  manifest-only edit has no build to complete, so it would need a separate path; the external gate
  unifies both — every change marks the deploy, the gate decides when it may run.
- **One shared parallelism budget for both phases.** Rejected: health-gate-waiting deploys would
  consume slots that builds want, partially defeating the overlap. Separate budgets keep the CPU
  cap on builds while letting cheap deploys wait freely.
