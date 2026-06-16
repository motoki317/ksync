---
date: "2026-06-16"
author: "motoki317"
status: "accepted"
---

# Context

ksync's build integration (20260612-build-integration) turns a `build:` entry's local sources into a
content-addressed dev image and injects the tag before sync. That is the right default for the inner
loop — edit source, get it deployed — but it assumes ksync is the thing that builds.

Some callers already have the image. A wrapper script that fronts ksync may resolve a ref per service
its own way: build it with a multi-target `docker buildx bake`, `docker pull` a registry tag, reuse a
pinned digest, or pick a CI artifact. In that flow building is the wrapper's job (and it also makes
the image visible to the cluster — a shared daemon, a `k3s ctr import`); ksync should not build the
same source a second time. It should deploy the supplied ref instead.

Concretely this is what lets a `just stack::up`-style wrapper (resolve all image tags, hand them to
ksync, call `ksync sync`) replace a bespoke deploy: ksync provides the ArgoCD-parity render → inject
→ sync, the wrapper owns image resolution.

# Decision

Add **image overrides**: a map of `image name → supplied ref` that, for any `build:` entry whose
`image` matches, replaces building with injecting the supplied ref.

- **Sources.** The `KSYNC_IMAGE_OVERRIDES` env var (whitespace/newline-separated `IMAGE=REF` tokens,
  so a script can emit one per line) and a repeatable `--image IMAGE=REF` flag on `sync` and `watch`.
  A flag wins over the env for the same image. `IMAGE` is the build entry's `image:` value exactly
  (the globally-unique key config already enforces); `REF` is a bare tag, `:tag`, `name:tag`,
  `@sha256:…`, or `name@digest` — a differing name redirects the registry/repo (kustomize `newName`),
  otherwise only the tag/digest changes.
- **Skip the build.** An overridden entry is never built. In one-shot `sync` it is dropped from the
  eager-build set; in `watch` its sources are not watched at all, so a source edit triggers neither a
  build nor a redeploy (a manifest edit still redeploys, re-injecting the override). An app whose
  every build is overridden needs no builder and no `imageLoad` — both are part of the build path the
  override bypasses, so making the image visible to the cluster is the supplier's responsibility.
- **Inject at deploy.** The supplied ref flows through the same `render.SetImages` path as a built
  tag — including the `Always → IfNotPresent` rewrite, since the supplier loaded the exact ref
  locally and a pull would be wasteful or doomed.
- **Unknown images are dropped, not fatal.** An override naming an image no app builds is skipped
  with a logged note. This decouples the wrapper from ksync's build list: it can hand ksync its full
  resolved image set and the surplus is simply unused, rather than forcing the two lists to stay in
  lockstep.

# Consequences

- A wrapper can own image resolution end to end and use ksync purely as the deploy engine: resolve
  refs (build/pull/pin) → make them cluster-visible → `ksync sync --image …` (or the env var).
- Mixed apps work: in one app, some images build from source while others are supplied — each entry
  is decided independently, and a deploy injects both built tags and overrides together.
- No persisted state and no new build invariants. Overrides are resolved per run from env/flags;
  "never deploy an unbuilt image" still holds because an overridden image is, by definition, already
  built by someone else.

# Impact

- `cmd/ksync`: `override.go` parses and validates overrides (`imageOverrides`, `parseOverrideRef`,
  the repeatable `stringSlice` flag). `runSync`/`runWatch` resolve them; `startEagerBuilds` skips
  overridden entries; `deployApp` injects override-or-built per entry.
- `internal/loop`: `Options.Overrides`; `buildState.override`; an overridden entry is excluded from
  `buildApps`, the watch mapping, and watch roots; the deploy builds its `[]render.Image` from
  override-or-tag. `runner.runDeploy` now takes the resolved image list directly.
- Tests: `parseOverrideRef`/`imageOverrides` units; `TestStartEagerBuilds_SkipsOverriddenImages`;
  `TestRun_OverriddenImageNotBuiltAndRefInjected`; `TestSetImages_Digest`.

# Alternatives

- **Pin any rendered image, not just `build:` entries.** More general (could pin a third-party image
  too) but it cannot distinguish a typo from an intentional pin of an image absent in the manifests —
  both silently no-op. Scoping to build images lets a mismatch be reported, and the feature's purpose
  is "supply instead of build," which is inherently about build entries.
- **Hard-error on an override with no build entry.** Rejected: it couples the wrapper to ksync's
  exact build list. A logged drop keeps the wrapper free to supply a superset; the note still
  surfaces a genuine typo.
- **A per-app/per-build config field instead of runtime input.** Rejected: the supplied ref is a
  property of *this run* (today's CI tag, a pull), not of the checked-in config; env/flags keep the
  committed `ksync.yaml` stable across runs.
- **Tag-only overrides.** Rejected for little savings: supporting `name`/`digest` too (via the
  existing kustomize `Image` fields) makes a digest pin or a registry redirect free.

# Notes

`internal/build`'s `Tag` doc already named "the render-time image override" — this is that path made
external. The override key is documented as the exact `image:` value; the wrapper integration emits
`<bare image>=<full ref>` per resolved service.
