---
date: "2026-06-23"
author: "motoki317"
status: "accepted"
---

# Context

The post-apply health gate (20260614-sync-health-gate) holds a sync open until every non-hook
resource is Healthy, or fails on `--timeout`. Two things around that gate were thin:

- **While waiting**, the live deploy row showed only a count: `waiting for health  N not ready`. The
  developer could see *that* the deploy was blocked, not *what* on — which app, edited and resynced,
  is now stuck behind which resource.
- **On timeout**, the error named the resources still not healthy and stopped there. To learn *why* —
  an `ErrImagePull`, a `CrashLoopBackOff`, a failed PostSync Job — the developer had to switch to
  `kubectl get events` and `kubectl logs` against the right cluster context and namespace. For an
  inner-loop tool whose whole point is a fast edit→deployed→broken signal, that is a jarring handoff.

A reference inner-loop wrapper this project dogfoods (a `stack` script) already prints failed
resources with their events and pod logs on a stuck deploy; ksync should give the same at-a-glance
read without the wrapper. The constraint: ksync is **not** a debug-first tool, so the dump must stay
small and scannable, not grow into a log viewer.

# Decision

Two additions, both around the health gate.

**1. Name the not-ready resources while waiting.** The gate's per-poll callback (`OnWait`) now
carries structured `ResourceStatus` values (a UI-neutral view — identity, status, message — with no
gitops-engine types leaked to the command layer), not pre-formatted strings. The live deploy row
renders the count plus the first few resources by short `Kind/name`, with a `+N` overflow:
`waiting for health  2 not ready: Deployment/api, StatefulSet/db`.

**2. Dump diagnostics on timeout.** The timeout is now a typed `engine.TimeoutError` carrying the
pending resources. The command layer detects it (`errors.As`) and calls `engine.Diagnose`, which
gathers and formats a bounded block:

- each still-unhealthy managed resource with its health verdict and (for non-pod kinds) its recent
  events — a Deployment's `ProgressDeadlineExceeded`, a PVC's pending reason;
- the **related pods**, found by walking the warm cluster cache's ownership hierarchy
  (`IterateHierarchyV2`) down from the unhealthy resources — a Deployment→ReplicaSet→Pod chain, a
  hook Job→Pod — with each pod's phase, per-container state (`Waiting (ImagePullBackOff)`,
  `Terminated (Error, exit 1)`), its own events (a `FailedScheduling`/`FailedMount` that left no
  container to inspect), and the tail of its **current and previous** container logs.

Every axis is capped hard (pods, events per object, log tail lines) so the block stays a glance, not
a log export. The dump runs on the parent (live) context, never the timed-out sync context — which
would make every events/logs read return at once and the dump come back empty — bounded by its own
short deadline so a wedged cluster cannot turn a sync timeout into a hang. It prints to scrollback as
pipeline output, not through the single-line logger, which cannot carry a multi-line block. The same
path serves `sync` and `watch`.

# Consequences

- A blocked deploy is legible live: the developer sees which app waits on which resource, not just a
  number.
- A timed-out sync explains itself in place — the unhealthy resource, its events, and the failing
  container's logs — covering the common inner-loop failures (image pull, crash loop, failed hook
  Job) without a context switch to `kubectl`.
- Reusing the warm cache's hierarchy index finds the related pods with no extra API calls; only the
  events, pod reads, and log tails hit the API, and only on the (rare) timeout path.
- The dump is bounded by construction, so it cannot flood the terminal — consistent with ksync being
  a sync loop, not a debugger.

# Impact

- `internal/engine`: new `ResourceStatus` value type and `TimeoutError`; `pending`/`unhealthyManaged`
  return `[]ResourceStatus` (with `pendingLines` kept as the unit-test seam); `SyncOptions.OnWait`
  takes `[]ResourceStatus`. New `diagnose.go`: `Diagnose`, the `IterateHierarchyV2` pod walk, event
  and log gathering, and the pure `formatDiagnostics` formatter.
- `cmd/ksync`: `deployWait`/`waitingTail` render the named wait line; `reportTimeoutDiagnostics`
  detects the typed error and prints the block (on the parent context) for both `sync` and `watch`.
- Tests: `TestFormatDiagnostics`, `TestFormatDiagnostics_OmitsEmptyLogs`, `TestContainerState`,
  `TestTimeoutError_NamesPending`, `TestWaitingTail`.

# Alternatives

- **Put the dump in the error string.** Rejected: the error flows through the single-line logger
  (which quotes embedded newlines into one escaped blob) and a multi-KB dump does not belong in an
  `error`. A typed error plus a separate printed block keeps the message terse and the dump readable.
- **List unhealthy pods by namespace instead of walking ownership.** Simpler, but it cannot tell an
  unrelated broken pod from one belonging to the stuck app. The hierarchy walk is precise and uses
  the cache already in memory.
- **Gather diagnostics inside `Sync` before returning.** Rejected: `Sync` only holds the timed-out
  context, so its own API reads would fail instantly. Diagnosing from the command layer on the parent
  context is what makes the reads succeed.
- **Stream full logs / run on every failure.** Rejected as over-reach: ksync is not a debug-first
  tool. Bounded tails on the timeout path only is the deliberate floor.

# Notes

The diagnostic includes **pod** events (where `ErrImagePull`/`FailedScheduling` actually surface)
even though the literal need was "resource events + pod logs" — they are the single highest-signal
item and cost one list call per pod. Resource-level events are skipped for pod-kind resources, since
the pod section already carries them.
