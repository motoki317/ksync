---
date: "2026-06-17"
author: "motoki317"
status: "accepted"
---

# Context

A long-lived dev cluster (a cold microVM, a CI scenario-test runner) pays a large up-front cost
re-pulling infra images over a slow link. The fix is a local image cache, but a cache is only
maintenance-free if it holds **exactly the images the deploy uses** — no more. The first cut
seeded the cache from "every image in the node's store minus a denylist", which bloats: across
runs the store accumulates stale tags (old versions, images of apps since removed) and the cache
grows without bound unless someone prunes it.

What was missing was a way to ask ksync, authoritatively, "which images does this config
deploy?" — so a cache (or a pre-pull step) can scope to that set and nothing else.

Two facts made this non-trivial:

- **Manifests use short refs.** `redis:7`, `alpine`, `localstack/localstack` are everywhere. A
  container runtime stores them normalized (`docker.io/library/redis:7`, …). A naive list of
  the raw strings would not match the store, so an intersection would silently keep nothing.
- **Some images are never in the manifest.** An ECK `Elasticsearch` names its data image only
  indirectly, via `spec.version`; the operator materializes
  `docker.elastic.co/elasticsearch/elasticsearch:<ver>` at runtime. The single biggest image in
  the reference workload was invisible to a render-only scan.

# Decision

Add a `ksync images` subcommand that prints the container images the selected apps deploy — one
canonical reference per line on stdout, sorted and deduplicated.

- **Canonical refs.** Each ref is normalized with `github.com/distribution/reference`
  (`ParseNormalizedNamed` + `TagNameOnly`) — the same normalization containerd applies — so the
  output matches a `ctr images ls` listing by plain string equality. The consumer needs no
  re-parsing and no normalization rules of its own.
- **Exclude built images.** Any `build:` entry's repository is dropped: those deploy as
  content-addressed local dev tags that exist in no registry and are never pulled, so caching
  them is pointless.
- **`--live` for operator-derived images.** The default reads only what the rendered manifests
  literally name (no cluster needed). `--live` additionally lists the images of running pods in
  the apps' namespaces (namespaces discovered from the render), via a plain client-go pod list —
  catching exactly the images a controller creates that no manifest names.

The extraction walks the standard pod-spec locations (containers / initContainers /
ephemeralContainers and OCI `image` volumes across Pod, workload templates, and CronJob) — the
same locations `SetImages` touches — in `internal/render` (`Result.Images()`).

# Consequences

A cache or pre-pull step can scope to `ksync images --live` and is then bounded by the config,
not by whatever the node accumulated: re-running it in CI never grows the cache past the manifest
set, with no pruning step. Measured on the reference workload: the cache holds the 34 images the
config deploys (including the operator's Elasticsearch image) and ignores injected stale tags; a
cold deploy from that scoped cache matches the unscoped one (~2m30s) with no large pulls.

`ksync images` (no `--live`) is also a useful building block on its own — pre-pull lists, image
inventories, policy checks — and needs no cluster.

# Impact

New read-only subcommand; no change to sync/apply behavior. `--live` issues one pod-list per
managed namespace against the allowlisted context (the same safety gate as every other command).

The render-only mode is honest about its limit: it reports only statically-named images, so any
controller-derived image needs `--live`. The list also reflects what is *declared*, not what a
node has pulled — a consumer still intersects with the actual store (a declared image whose pod
has not run, e.g. an unfired CronJob, simply is not there to cache).

# Alternatives

- **Raw (familiar) refs instead of canonical.** Friendlier to read, but pushes containerd's
  normalization rules into every consumer; the cache match is the primary use, so canonical wins.
- **Reuse `ksync render` + parse YAML downstream.** Re-implements image-field walking outside
  ksync and still misses operator-derived images and normalization.
- **Live-only (skip render).** Misses declared-but-not-running images (a CronJob whose pod was
  pruned) and needs a cluster even for a static inventory. The union (render ∪ live) is strictly
  more complete.
- **Match by image digest rather than ref.** Digests dodge normalization but break on multi-arch
  index images, where a pod's per-platform manifest digest differs from the stored index digest.
