---
date: "2026-06-27"
author: "motoki317"
status: "accepted"
---

# Context

When an app does not become healthy before its `--timeout`, ksync prints a
diagnostic block (`engine.Diagnose`, ADR 20260623-sync-timeout-diagnostics).
Dogfooding a 22-app stack surfaced two failures of that block.

First, a Ctrl-C produced garbage. The health-gate loop returned a `*TimeoutError`
on **any** `ctx.Done()`, but the sync context is `withTimeout(parent, --timeout)`
where the parent is the signal context and `runByNeeds` also cancels siblings on
the first error. So a Ctrl-C — or one app failing and aborting the rest — made
every other app report a timeout, and the command then gathered diagnostics
against that same already-cancelled context. Every pod read came back
`unreadable: client rate limiter Wait returned an error: context canceled`.

Second, even a real timeout's dump was bloated. A three-replica CrashLoop printed
four near-identical pods, each with five Normal events (`Scheduled`, `Pulled`,
`Started`), the redundant `BackOff` warning, and a `logs (previous):` block
holding only the kubelet's `unable to retrieve container logs for docker://…`
placeholder. The real cause — the exit code — was hidden, because a
CrashLoopBackOff container's current state is `Waiting (CrashLoopBackOff)` and the
exit code lives in its last-termination state, which the dump never read.

# Decision

Separate interruption from timeout, and make the dump terse.

- The health-gate and apply loops distinguish `ctx.Err()`: `DeadlineExceeded`
  stays a `*TimeoutError`; any other cause returns the cancellation. The command
  boundary maps `context.Canceled` to a single `ksync: interrupted` line and exit
  130 (not the cryptic raw error).
- A one-shot `ksync sync` gathers the dump on a **user interrupt** too, not only on
  a timeout. The usual way to abandon a wedged deploy is Ctrl-C — the user hits it
  rather than wait out a multi-minute `--timeout` — and still wants to see why it
  was stuck. The gate is the *signal* context, not the app's own cancelled context:
  `runByNeeds` also cancels in-flight siblings when one app fails, and those
  aborted-but-progressing apps must stay quiet (the failed app reports its own
  error; piling their incomplete state on top is noise). `Diagnose` self-selects
  the still-unhealthy resources, so an interrupted app that was merely mid-rollout
  prints nothing. The `watch` loop never gathers on cancellation: there Ctrl-C is a
  routine quit of a long-running process, not a debugging moment.
- `Diagnose` runs on a fresh `context.Background()` bounded by `diagnoseTimeout`,
  never the sync's own cancelled context, so the reads always complete whether the
  trigger was a timeout or a Ctrl-C. The second Ctrl-C still force-quits (ADR
  20260627-double-signal-force-quit) before that bounded gather finishes, so the
  wait is always escapable.
- The dump is reduced to the actionable signal: events filtered to warnings
  (Normal events are noise), then deduped — identical messages merged, and per
  reason the single most informative kept (an ImagePullBackOff's three `Failed`
  events collapse to the one naming the real cause); the `BackOff` warning dropped
  as a restatement of the container state. Identical pod failures (a Deployment's
  replicas) collapse to one representative plus a count. Each pod shows one log
  stream (the previous run for a crashed container, else current), with the
  kubelet placeholder filtered out. A container's last-termination is appended to
  its waiting state, so a CrashLoopBackOff names its real exit code or OOMKill.
  Init containers are listed and a failing one is tailed in preference to the app
  containers it blocks.
- The gather is also bounded in *work*, not just output: each related pod costs a
  live get, an event list, and a log stream, so a large failing ReplicaSet (or
  several apps interrupted at once) is capped at `diagMaxPodScan` pods inspected —
  enough for dedup to find the distinct modes without spending the whole budget to
  print at most `diagMaxPods` of them. Both caps (pods scanned, distinct modes
  shown) name what they dropped rather than imply the dump was exhaustive.
- Intermediate controllers (a ReplicaSet, a Job) between a managed root and its
  pods are surfaced when they carry a warning the pods cannot — the "no pod
  created" failure class. A `ResourceQuota` blocking pod creation puts the only
  actionable signal (`FailedCreate: exceeded quota`) on the ReplicaSet; there is
  no pod for the ownership walk to reach. Warning-only filtering keeps controllers
  whose pods came up fine (Normal `SuccessfulCreate` only) out of the dump.

# Consequences

A Ctrl-C reads as an interrupt, not a wall of false timeouts — yet it still prints
why each stuck app was wedged, because aborting a one-shot sync is exactly when the
user wants that. Whether the trigger is a timeout or a Ctrl-C, the dump is a few
lines that name the cause: the container state, the one log worth reading, and the
warning events that add to them. Replicas no longer multiply the output, and when
the distinct failures exceed the per-run cap the dump says how many it dropped
rather than implying it showed them all.

# Impact

`internal/engine/diagnose.go` is rewritten; `internal/engine/sync.go` gains the
cancel/timeout split; `cmd/ksync` maps the interrupt and gathers on a fresh
context. The pure formatter and dedup/filter logic stay unit-tested. Live
fixtures for the failure modes live in `testdata/diagnostics/` for manual
regression-checking against a local cluster.

The dedup heuristic is lossy by design — collapsing per-reason events and
identical pods can hide a genuinely different same-reason event or a replica that
differs only subtly. The bound favors brevity (the stated goal: when two outputs
carry the same information, the shorter wins); the full state is always a
`kubectl` away.

# Alternatives

- **Keep diagnostics on the sync context, fix only the cancel split.** A sibling
  app failing mid-gather would still cancel the reads. The fresh context removes
  the race entirely.
- **Show every pod and every event, let the reader filter.** This is the behavior
  being replaced; a 22-app stack made it unreadable.
- **Suppress the dump unless a single app failed.** Loses the common multi-app
  timeout case, which is exactly when a dump is most useful.
