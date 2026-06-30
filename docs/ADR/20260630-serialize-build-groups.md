# Serialize build-group commands

Date: 2026-06-30
Status: accepted

## Context

Bulk build groups (2026-06-14) hand a group's dirty members to one command. The coalescing is
**per-app**: `App.BuildBatches` partitions one app's build entries, and the loop builds apps
concurrently (eager build-ahead, 2026-06-16). So when two apps' build entries name the *same* group,
each app runs the group's command itself, and those invocations run **at the same time** — one per
app, not one for the group.

A group's bulk command need not be concurrency-safe. The host-compile shape is the clear case:
`cargo zigbuild` lazily creates a shared wrapper cache (`~/Library/Caches/cargo-zigbuild/<ver>/`) on
first use, and two simultaneous *cold* invocations race to create it — one wins, the other fails with
`File exists (os error 17)` and leaves its image unbuilt. The same race can instead leave both
invocations reporting `Finished` while one produces no binary.

This is the same class of hazard the imageLoad Loader (2026-06-13) already serializes for: `k3d image
import` is not concurrency-safe either. Loads funnel through one serialized Loader; build-group
commands had no such gate.

It stayed hidden because it needs a *cold* cache and concurrency at once. Local and microVM runs keep
a warm, persistent cargo-zigbuild cache, so the create-race never fires (a single build, and a
concurrent build on a warm cache, both pass — verified). A CI runner whose caches do not persist is
cold on every run, so the race fired on the first scenario run that built two grouped apps from
source.

## Decision

Serialize a build group's command across apps by default: at most one invocation of a given group
runs at a time. A new `build.GroupGate` holds a per-group mutex and wraps the `BuildGroup` call in
`makeBuildFunc` (the one closure shared across the concurrent per-app builds), mirroring the single
shared Loader.

A group whose command *is* safe to run concurrently with itself opts out with `parallel: true` on the
`BuildGroup`. Different groups, and ungrouped builds, never block each other.

## Consequences

A non-concurrency-safe bulk command — a cold host compiler, a cache-creating tool — is safe by
default, with no per-environment workaround (no `flock` in the command, no cache pre-warm step before
the build fan-out). The flaky, environment-dependent failure cannot occur unless a config explicitly
opts into `parallel`.

The cost is parallelism: two apps that share a group build one at a time. That is the intended
trade for safety; a group whose command is concurrency-safe (a plain `docker buildx bake` of
independent targets) sets `parallel: true` to keep the overlap.

## Impact

Scoped to apps that share one group; nothing else changes. The gate serializes the whole group
command (including any `bake` inside it), not a sub-step — coarser than locking only the unsafe step,
but the command is the unit ksync invokes, and a finer lock would have to live inside the
user-supplied command. The gate uses a plain per-group mutex, not a ctx-aware wait, matching the
Loader; a wedged build is still escapable via the second-Ctrl-C force-quit (2026-06-27).

## Alternatives

- **Leave it to the integration** — `flock` around the unsafe step, or pre-warm the cache once before
  the build fan-out. Keeps full parallelism, but pushes a non-obvious concurrency requirement onto
  every config that shares a group, and ksync already owns exactly this concern for loads. Pre-warm
  additionally relies on the cache being cold-only and cannot live inside the (concurrent) group
  command — it would need a separate step ahead of the build phase. Rejected as the default.
- **Coalesce a group across apps into one invocation** (as the Loader coalesces waves) — also removes
  the race and amortizes per-invocation cost, but changes build semantics (one command builds several
  apps' images) and is a larger change. Serialization alone fixes the bug; coalescing is deferred.
- **Serialize unconditionally, no opt-out** — needlessly serializes a concurrency-safe bulk builder.
  Hence `parallel`.

## Notes

The default is serialize, not parallel, because more bulk commands want it than not (host compilers,
tools that build a shared cache), and the safe default trades some parallelism to avoid a silent,
flaky, environment-dependent failure. A config recovers the parallelism deliberately, per group.
