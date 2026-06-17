---
date: "2026-06-17"
author: "motoki317"
status: "accepted"
---

# Context

`imageLoad` makes a freshly built image visible to a cluster with a separate image store (k3d,
kind, k3s-in-a-VM, a remote registry). The previous design (ADR 20260615) ran the command from
each parallel per-app build goroutine and let a `*bool allowParallel` decide whether those runs
could overlap: it defaulted to `true` (concurrent, fast) and a user set it `false` for a
non-concurrency-safe importer — `k3d image import`, whose shared per-cluster tools node and
second-resolution tarball name collide when two imports overlap, silently dropping images behind
a green ✓.

Two things were left on the table. First, the safety knob is a footgun: the dangerous default is
the *fast* one, so a k3d user who does not know to set `allowParallel: false` gets intermittent,
silent image loss — the worst failure mode. Second, the load tools that matter (`k3d image
import`, `kind load`, `k3s ctr import`) all accept **many images in one invocation**, and their
cost is dominated by fixed per-call overhead (spin up a tools node, ship a whole tarball — seconds,
regardless of image count). ksync already passed a whole *build group* as one `$KSYNC_IMAGES`
batch, but loads from **different** apps/groups that finished around the same time still ran as
separate invocations — paying the fixed cost once per app. Verified live: `k3d image import a b`
loads both in one ~4s tools-node pass; two separate imports pay ~4s twice.

# Decision

Drop `allowParallel`; **always serialize** the load step and **coalesce** what piles up.

`build.Loader.Load` no longer has a `Parallel` field. Internally it holds one load slot: the first
caller runs its own `$KSYNC_IMAGES` immediately; any caller that arrives while a load is in flight
parks in a queue instead of running. When the in-flight load finishes, the running goroutine drains
the whole queue into **one** invocation — `$KSYNC_IMAGES` carrying every queued ref, the command's
output tee'd to each parked app's import row — and delivers that wave's result to all of them. It
repeats until the queue is empty, so loads never overlap and stragglers still batch.

`imageLoad` config becomes just `{ command }` (the `allowParallel` field is gone). A bulk-capable
command (`k3d image import $KSYNC_IMAGES`) benefits directly; a command that cannot take many
images at once loops over `$KSYNC_IMAGES` itself (`for i in $KSYNC_IMAGES; do docker push "$i";
done`).

# Consequences

- The silent k3d image-loss mode is gone *by construction*, with no flag to get wrong:
  serialization is unconditional. Verified — three apps built in parallel against a real k3d
  cluster produced **two** imports (leader alone, then the other two coalesced into one
  `k3d image import a b`), and all three pods came up `Running` on the locally built tags.
- Cold/bulk convergence is faster than one-import-per-image: the per-call fixed cost is amortized
  across whatever has piled up behind the running load. A steady-state single-edit loop has nothing
  to batch and behaves exactly as before (one image, one import).
- A previously-parallel loader (registry push, `kind load`) is now serialized. For those the loss
  is real but small — a cold multi-image sync pushes/loads sequentially instead of concurrently —
  and is the deliberate price of one safe, knob-free model. Batching recovers most of it for any
  command that accepts multiple images at once (a single `docker push` of distinct images, a single
  `kind load` of several).

# Impact

- Breaking config change: `imageLoad.allowParallel` no longer parses — remove it. `imageLoad` is
  now `{ command: <string> }`. In-repo docs and both dogfooding configs (k3d, k3s-in-VM) were
  migrated; the k3s config switches its per-image `docker save … | ctr import` loop to the bulk
  `docker save $KSYNC_IMAGES | k3s ctr -n k8s.io images import -` form, which coalescing now feeds
  multiple images.
- `build.Loader` loses its `Parallel` field and `config.ImageLoad` loses `AllowParallel` /
  `Parallel()`. Supersedes ADR 20260615.
- The leader goroutine runs the coalesced wave on behalf of the parked callers, so a wave's wall
  clock is shared: a caller's import row shows elapsed inclusive of the time it waited to be picked
  up. Accurate to wall-clock, as before.

# Alternatives

- **Keep `allowParallel`, add batching on top.** Two overlapping mechanisms (a concurrency knob
  *and* a coalescing queue) for one concern. Once loads coalesce, parallelism buys little and the
  knob's only live setting was `false` (k3d) anyway — so the safe path is also the simple path.
  Dropping the knob is strictly less to get wrong.
- **Default `allowParallel: true` but auto-detect k3d.** Rejected in 20260615 and still rejected:
  ksync carries no per-cluster-type knowledge.
- **Promote a parked waiter to run the coalesced wave** (so the original leader returns the instant
  its own import finishes, shaving its deploy's wait). A real latency win for the first app, but it
  adds a promotion handshake for a saving that only helps whichever app happened to import first;
  the simpler leader-drains model keeps all apps finishing together and is easier to reason about.
  Revisit only if first-app deploy latency on a cold bulk start becomes a real pain point.

# Notes

Why serialization is safe for *every* loader even though only k3d corrupts: the cost of serializing
a concurrency-safe loader is recovered by coalescing (one command, many images), so there is no
loader for which "always serialize + coalesce" is worse than the old "parallel" by more than the
gap between concurrent and batched-sequential execution of the same images — small, and paid only
on a cold multi-image start.

The k3s bulk form relies on `docker save img1 img2 …` producing one tarball whose `manifest.json`
lists every ref (verified), which `ctr -n k8s.io images import -` reads in full. The `-n k8s.io`
namespace is required — kubelet resolves pod images from containerd's `k8s.io` namespace, not the
default one.
