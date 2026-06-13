# Content-address dev tags by image fingerprint, not the image ID

Date: 2026-06-14
Status: accepted

## Context

The build-integration ADR (2026-06-12, decision 2) makes ksync's dev tag content-addressed by
reading the built image's **ID** and tagging `<image>:ksync-<12 hex of the ID>`. Unchanged source
hits the layer cache, yields the same ID, the same tag, an unchanged pod spec — no rollout. The ADR
notes one daemon behavior to defeat: the default provenance attestation embeds build timestamps, so
ksync passes `--provenance=false` (and advised `command` builds to do the same).

Validating bulk build groups against a real `docker buildx bake` project (and then in isolation)
showed `--provenance=false` is **not sufficient** for bake, and neither is `--set '*.attest='`,
`SOURCE_DATE_EPOCH`, nor `rewrite-timestamp`: two rebuilds of byte-identical content produce
**different image IDs**. Inspecting the two images shows why — identical `RootFS.Layers` (the
filesystem is reproducible) and an identical top-level `Created`, but a different config digest,
because BuildKit stamps per-build timestamps into the image config's `history` entries. The config
digest *is* the image ID, so the ID drifts even though nothing meaningful changed.

The consequence is severe for the `command`/bake escape hatch (which both target monorepos use):
every build gives every image a new ksync tag, so every sync rolls every pod — including unchanged
ones. Measured directly: editing one of four bake services rolled all four. ksync's own
`docker build --provenance=false` does not have this problem (its ID is stable), so the issue was
invisible until the bake escape hatch was exercised under content addressing.

## Decision

Derive the dev tag from an image **fingerprint** instead of its ID: hash
`{{json .Config}}{{json .RootFS.Layers}}` (the runtime config — Entrypoint/Cmd/Env/Labels/… — plus
the layer diffIDs) and tag `<image>:ksync-<12 hex of the hash>`. Both parts were verified stable
across rebuilds of identical content and sensitive to real change:

- **Layer diffIDs** are the reproducible fingerprint of the filesystem: identical across rebuilds,
  different the moment any file changes. They carry no timestamps.
- **`.Config`** (as `docker image inspect` exposes it) is the OCI image config's runtime section —
  Entrypoint, Cmd, Env, WorkingDir, ExposedPorts, User, Volumes, Labels. It contains none of the
  drifting `created`/`history` timestamps (those live in fields `docker image inspect` omits), so it
  is stable across rebuilds, yet it captures config-only changes the layers miss — notably an
  `ENTRYPOINT` edit on an image whose filesystem is unchanged. NeoShowcase's Go components share one
  compiled binary and differ *only* by `ENTRYPOINT`; layers alone would tie their tags together, so
  including `.Config` is load-bearing, not belt-and-suspenders.

Mechanically this also simplifies the build path: ksync already retags from a temp tag
(`<image>:ksync-build`) because the containerd image store does not resolve config digests in
`docker tag`. It now inspects that temp tag for the fingerprint and drops the `--iidfile` entirely,
so the single `docker build`, the `command` build, and each member of a build group all share one
"inspect fingerprint → hash → retag" path.

## Consequences

- The no-roll-on-unchanged guarantee now holds for **any** builder, reproducible image ID or not —
  most importantly `docker buildx bake`. Measured after the change: editing one of two bake services
  rolled only that service; a no-change re-sync applied nothing.
- `--provenance=false` on ksync's own `docker build` is kept (it is still correct and avoids an
  attestation manifest), but it is no longer what the guarantee depends on.
- No persisted state, as before: the fingerprint is recomputed from the freshly built image on every
  build, so a restart still converges.

## Impact

- A theoretical collision (two genuinely different images hashing the same 12 hex) stays as
  irrelevant as it was for the 12-hex-of-ID scheme — and tags are per image name regardless.
- If a build deliberately bakes a per-build value into `.Config` (e.g. a label set to the build
  timestamp), that *is* a content change by ksync's definition and will roll the pod. This is the
  user's choice in their build, not ksync drift; the common case (no such label) is stable.
- Fingerprint bytes depend on the docker version's `inspect` output shape; across a docker upgrade
  the tags may change once. Harmless given there is no persisted state — one convergence, then
  stable again.

## Alternatives

- **Keep the image ID; tell users to make bake reproducible.** Rejected: no flag combination tried
  (`provenance=false`, `attest=`, `SOURCE_DATE_EPOCH`, `rewrite-timestamp`) stabilized the bake ID,
  and pushing an unsolved reproducibility burden onto every bake user contradicts the escape hatch's
  point.
- **Hash layer diffIDs only.** Simpler, but blind to config-only changes (an `ENTRYPOINT`/`ENV` edit
  with an unchanged filesystem would not roll), which the shared-binary multi-entrypoint pattern
  makes a real case.
- **Hash the whole config blob including history.** That is exactly the drifting image ID; it brings
  back the spurious rolls.
