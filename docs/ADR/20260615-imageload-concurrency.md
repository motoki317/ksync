# imageLoad concurrency: serialize unsafe loaders, structured config

Date: 2026-06-15
Status: accepted

## Context

`ksync sync` builds apps in parallel and, per the image-load-hook ADR (2026-06-13), runs the
`imageLoad` command once per build batch to make the freshly built images visible to a cluster
with a separate image store. The hook ran from each parallel per-app build goroutine, so several
`imageLoad` commands could execute at once.

Dogfooding a k3d project surfaced that this is unsafe for `k3d image import`. A bulk sync that
fired four imports concurrently landed only two images; the other two apps' pods sat in
`ErrImageNeverPull` (`imagePullPolicy: Never`, the content tag absent from the node) and the sync
timed out at the health gate — yet every `📦` line reported success. `k3d image import` stages
every import through a single shared per-cluster *tools* node and a tarball named only to the
second (`...-images-<YYYYMMDDhhmmss>.tar`) in a shared volume, then deletes "the tarball(s)" and
the tools node on cleanup. Two imports overlapping in time therefore collide on the tarball name
and each one's cleanup removes the other's tarball / tools node, so some import nothing while
still exiting 0.

This is not universal: a registry push (`docker push`) is built for concurrent pushes of distinct
images, the shared-daemon case (Docker Desktop) runs no command at all, and `kind load` stages
through a per-invocation temp dir. The defect is specific to loaders that funnel through shared,
non-reentrant scratch — `k3d image import` being the notable one. So a blanket "always serialize"
would needlessly slow the concurrency-safe loaders, and "always parallel" silently corrupts k3d.

## Decision

Two parts.

1. **`build.Loader` serializes by default; the command layer opts into concurrency.** `Loader`
   becomes a single shared instance whose `Load` holds an internal mutex across the command's
   execution unless its `Parallel` field is set (the per-call progress writer moved to a `Load`
   parameter so one instance can serve every parallel build). The zero value is safe — serialized
   — so the library never corrupts a cluster by default; the application chooses the policy.

2. **`imageLoad` becomes a structured field carrying that policy.** It changes from a bare command
   string to an object:

   ```yaml
   imageLoad:
     command: k3d image import --cluster dev $KSYNC_IMAGES
     allowParallel: false
   ```

   `allowParallel` **defaults to true** (`ImageLoad.Parallel()` returns true when the field is
   absent). The common loaders are concurrency-safe — registry push, shared daemon — so they keep
   the fast default and write no flag; only a k3d-style importer sets `allowParallel: false`. The
   command layer passes `cfg.ImageLoad.Parallel()` to `Loader.Parallel`, so the unsafe case
   serializes and everything else stays parallel. Builds themselves always run in parallel; only
   the load step is gated.

## Consequences

- The k3d image-loss bug is gone with no config change required for correctness *if* the user
  marks the command — and the failure mode it replaces (a silent, intermittent ✓ over missing
  images) is the worst kind, so a slower-but-correct default for that command is the right trade.
- Concurrency-safe loaders keep full parallelism; the only cost of serialization falls on a cold
  multi-image `sync`/`watch` with `allowParallel: false`, where loads run one at a time. A
  single-image edit (the steady-state loop) has nothing to serialize, so it is unaffected.
- Serialized `📦` progress lines show queue-inclusive elapsed on a cold bulk sync (a later import
  includes the time it waited behind earlier ones). Accurate to wall-clock; only the bulk case.

## Impact

- Breaking config change: `imageLoad: <string>` no longer parses; it must be
  `imageLoad: { command: <string>, allowParallel?: bool }`. All in-repo configs, docs, and the
  dogfooding configs were migrated. `allowParallel` is `*bool` so "absent" (default parallel) is
  distinguishable from an explicit `false`.
- `Loader.Load` gains an `out io.Writer` parameter (was a struct field) so one shared instance
  serves all builds; existing call sites and tests updated.

## Alternatives

- **Always serialize (the first fix).** Simple and always correct, but penalizes registry-push
  users on every cold sync for a defect that is not theirs. Rejected for the opt-out.
- **Default parallel, opt *out* via `serial: true`.** Same capability, negative framing.
  `allowParallel` was preferred as the positive form; default stays parallel either way.
- **Coalesce all images into one `imageLoad` call.** Sidesteps concurrency (one `k3d image import
  a b c`) and is faster for k3d, but forces every build to finish before any image loads, breaking
  the per-app build→load→apply pipeline `needs` relies on. Rejected.
- **Detect the loader (special-case "k3d").** Violates the hook's whole premise that ksync carries
  no per-cluster-type knowledge. The user declares safety instead.
