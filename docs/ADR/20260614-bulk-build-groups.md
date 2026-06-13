# Bulk build groups

Date: 2026-06-14
Status: accepted

## Context

The build-integration ADR (2026-06-12) builds one image per `build` entry: a `docker build`, or a
`command` escape hatch that leaves `$KSYNC_IMAGE` in the daemon. The image-load ADR (2026-06-13)
loads each built ref once, serially, and explicitly deferred batching ("Revisit only if many-image
cold start becomes a real pain point").

Dogfooding against two real monorepos made that the pain point. Both build many service images that
share most of their work, and both already drive `docker buildx bake`:

- **One Dockerfile, many bake targets (k3d cluster).** A single `docker-bake.hcl` builds ~7 services
  from one shared `builder` stage (the compile) plus a thin per-service final stage. Modeled as one
  `build` entry per image, an edit to shared source dirties every service and ksync runs N separate
  `docker buildx bake <one-target>` invocations *serially*, then N separate `image import`s.
  BuildKit's layer cache saves the recompile, but the per-invocation overhead, the serialized
  `--load` exports, and N cluster imports do not collapse. One `docker buildx bake t1 t2 …` would
  compute the shared stage once and export the targets in one BuildKit session; one
  `image import a b c` would replace N imports.

- **Host-compile then thin image.** Another monorepo pre-compiles binaries on the host (one workspace
  build per service) and the Dockerfiles only `COPY` them in. The natural shape is one bulk command —
  prebuild the dirty services, then bake all of them — not N independent per-image commands each
  re-entering the toolchain.

Both want the same primitive: *when several images are dirty, hand them to one command that builds
them together*, while keeping ksync's content-addressed tags (no rollout on unchanged source) and
its cluster-agnostic transport.

## Decision

Add **build groups**: a top-level `buildGroups: [{name, command}]`, and an optional `group` field on
a `build` entry naming the group it joins. A grouped entry sets neither `command` nor `dockerfile`
(the group's command builds it); its `context` and `watch` still scope what dirties it.

1. **The loop batches each app's dirty entries by group.** `App.BuildBatches` turns the dirty entry
   indices into batches: every ungrouped entry is its own batch (the existing per-image path),
   and a group's dirty members coalesce into one batch — only the *dirty subset*, which may be a
   single image. `BuildFunc` now takes a batch (`[]config.Build` → `[]string`) instead of one entry.

2. **The group command receives `$KSYNC_IMAGES`.** It is the newline-separated temp refs
   (`<image>:ksync-build`) the command must leave in the daemon — the plural of the single-build
   `$KSYNC_IMAGE` contract. ksync then content-tags each result the same way a single build is
   tagged, so per-image content addressing and the no-roll-on-unchanged property hold: a bulk bake
   of 6 targets where only 2 changed rolls only those 2. The command runs in the group's shared
   context (validated to be one directory).

   Validating this against a real bake surfaced that `docker buildx bake` does not produce a stable
   image ID for unchanged content (it stamps per-build timestamps into the image config even with
   attestations off), which would roll every image on every build. The fix — content-addressing
   from the image *fingerprint* (config + layer diffIDs) rather than the drifting config ID — is its
   own decision, [20260614-content-address-by-fingerprint](20260614-content-address-by-fingerprint.md);
   it is what makes a group's incremental rebuild roll only the images that actually changed.

3. **`imageLoad` loads a batch in one invocation.** It now also gets `$KSYNC_IMAGES` (and
   `$KSYNC_IMAGE` stays, holding the first ref, for the single-image case). A batch's refs import
   together — `<tool> image import a b c` — instead of one import per image. Newline separation means
   an unquoted `$KSYNC_IMAGES` word-splits into one argument per image.

4. **Cross-app group members build per app, no scheduler change.** Batching happens inside one app's
   run. If a group's members span apps (a service deployed on its own may sit in a different app from
   the bulk of the bake targets), each app bulk-builds *its own* members; BuildKit's cache still
   shares the compile across the two bakes. This keeps the per-app scheduler (serialization, `needs`
   gating, parallelism) exactly as is. Coalescing a group across apps into a single command is
   possible later as a loop-level change without a config change.

## Consequences

- A shared base image / compiler pass / cluster import runs once per batch instead of once per
  image. The hot path is unchanged for single-image edits (a one-member batch is still one command),
  and manifest-only edits still never invoke docker.
- The bulk command is still tool-agnostic: ksync sets `$KSYNC_IMAGES` and content-tags the results;
  it carries no knowledge of bake, host compilers, or any builder. The user writes a small wrapper
  that maps the temp refs to their builder's targets (for bake: strip `:ksync-build` and the registry
  prefix to get the target name) — a minor, one-time project change, accepted as the cost of
  generality.
- `BuildFunc`'s signature changed to a batch. Internal only (the loop and `cmd/ksync` compose it);
  no config-visible change for users who do not adopt groups.

## Impact

- A grouped entry delegates building to the group, so it must not also set `command`/`dockerfile`;
  config validates this, that every `group` references a declared group, that a group has at least
  one member, and that all of a group's members share one context (the command's working directory).
- To benefit from bulk import, the user updates `imageLoad` from `$KSYNC_IMAGE` to `$KSYNC_IMAGES`.
  `$KSYNC_IMAGE` keeps working (single ref), so existing configs are unaffected until they opt in.
- Groups are orthogonal to `needs` and to the one-build-definition-per-image rule; both still hold.

## Alternatives

- **Infer groups from identical commands.** Coalesce entries whose `command` matches. Rejected:
  per-target `--set` differences make commands not identical, and implicit coalescing hides what runs.
- **One bulk command per app (no group field).** Too coarse: an app can mix plain `docker build`
  entries with bake targets in one bulk-able set, and a single per-app command cannot express that;
  `watch` scoping is also per image.
- **Cross-app group coalescing in v1.** It would split the build phase out of the per-app run and
  rework the scheduler for a marginal gain over per-app batching (BuildKit already shares the
  compile across app bakes). Deferred; the config surface chosen does not preclude it.
- **Batch loads only, keep per-image builds.** Solves the import cost but not the serialized builds
  or the per-invocation overhead, and does not fit the host-compile-then-bake shape at all.

## Notes

The single and bulk contracts stay deliberately symmetric: build *produces* `$KSYNC_IMAGE(S)`, load
*consumes* it. A reader who understands the one-image `command` understands the group `command` — it
is the same contract with an `S`.
