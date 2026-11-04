---
date: "2026-11-04"
author: "motoki317"
status: "accepted"
---

# Context

Only one app can build an image (config validation), and until this decision only that app
deployed the image at its dev tag. Another app that runs the same image deployed the tag that its
manifests name, and pulled it from a registry. For example, app `team-a` builds `ghcr.io/app/seed`
and runs it, and app `api-b` runs it in a seed-data Job. `api-b` then ran a released image, not
the local source. When the registry has no floating tag such as `main`, the manifest pins a fixed
tag, and that tag goes stale with every source change.

# Decision

An app gets the dev tags of the build entries of the apps that it lists directly in `needs`.
Below, `api-b` lists `team-a` in `needs`, and `team-a` builds `seed`.

- **Ref order.** For each build entry of each app in `needs`, `api-b` deploys the first of:
  1. the override for the image (`--image` or `KSYNC_IMAGE_OVERRIDES`),
  2. if `team-a` is selected, the dev tag that `team-a` built last in this sync or watch,
  3. if `team-a` is not selected, the dev tag that `team-a`'s live resources run,
  4. the manifest ref. If `api-b`'s manifests reference the image, ksync prints a note.

  In cases 1 to 3, `SetImages` injects the ref, so `imagePullPolicy: Always` becomes
  `IfNotPresent` as for `team-a`. An injected image that `api-b` does not reference changes
  nothing.
- **One list of needed builds.** `Config.NeededBuilds(app)` lists the build entries that an app
  gets from its needs. It reads every app in the config, so a need that is not selected still
  contributes its build entries.
- **watch.** When a build gives a `team-a` entry a new tag, `api-b` re-syncs with that tag. The
  needs gate starts it after `team-a` redeploys and is healthy. `api-b` is skipped if its last
  render did not reference the image. If `api-b` has no render yet, or a deploy of it is in
  flight, ksync cannot tell whether it references the image, so `api-b` re-syncs.
- **A need that is not selected.** With explicit app names, `ksync sync api-b` does not sync
  `team-a`, and the help already assumes that `team-a` is deployed. ksync reads the dev tag of
  `seed` from `team-a`'s live objects in the warm cache, and adds it to the run's overrides. It
  takes the tag only if every occurrence agrees. The image needs no `imageLoad`, because the
  cluster already runs it.
- **diff** previews a sync. An override, including the live tag above, shows its ref. If
  `team-a` is selected, an image that `team-a` builds and `api-b`'s manifests reference shows the
  dev tag that `team-a`'s live objects run. Sync deploys that tag to `api-b` when `team-a`'s
  source is unchanged, even if `api-b` still runs an older one. If `team-a` runs no single dev
  tag, diff carries forward the tag that `api-b`'s live objects run, as for an own build entry. If
  `team-a` is not selected, diff shows the override, the live tag above, or the manifest ref, as
  sync does. It never carries forward `api-b`'s live tag, because sync never builds the image.
- **images** leaves out a built image only for its owner and the apps that need the owner. An app
  that references `seed` without the needs edge pulls it from a registry, so `images` lists it.
  `--live` still drops every built image from the live pod images. This partly supersedes
  "Exclude built images" in [20260617-images-command](20260617-images-command.md).

# Consequences

`api-b` runs the image built from local source, so its manifests need no registry tag for the
image. A placeholder tag is enough while `team-a` is selected or deployed.

# Impact

- watch detects a reference with `Result.ReferencedRepos`, which finds images only under an
  `image` key and in `image.reference`. ksync injects a tag into a custom resource field that
  `configurations:` declares under another key, but it does not re-sync `api-b` when that tag
  changes. diff misses the same reference, so it shows the manifest ref against the live dev tag.
- In `watch`, the live tag of a need that is not selected holds for the whole session, because
  ksync does not watch `team-a`'s sources. It goes stale if another process redeploys `team-a`.
  Restart watch, or select `team-a`.
- `images` leaves out the manifest ref that `api-b` deploys in case 4.

# Alternatives

- **Give the dev tag to every app that references the image**, as Tilt matches a build to every
  resource that names the image. Rejected: without a needs edge, an app can deploy before the
  image is built, and the change of behavior is not opt-in.
- **Transitive needs.** Rejected: if `api-b` needs `team-a` only through `shop`, the needs gate
  holds `api-b` only for `shop`. When `shop` does not use `seed`, watch does not redeploy `shop`
  on a `seed` rebuild, so `api-b` can deploy the new tag before `team-a` is healthy. A direct edge
  costs one word.
- **A separate field that names the images an app uses.** Rejected: it must agree with `needs`,
  which already orders the deploy after the build.
- **Re-sync every app that needs the owner on any rebuild.** Rejected: an owner with eight images
  re-syncs an app that uses one of them on each rebuild of the other seven.
- **Keep the manifest ref when the need is not selected.** Rejected: a placeholder tag exists in
  no registry, so `ksync sync api-b` would deploy a broken ref.
- **Build the image of a need that is not selected.** Rejected: ksync would build, load, and watch
  the sources of an app that the user did not select.

# Notes

Skaffold's `requires` is the closest prior art: an artifact declares that it uses another
artifact's built image, and gets it at build time only
([artifact dependencies](https://skaffold.dev/docs/tutorials/artifact-dependencies)). The needs
edge is the same explicit declaration, at deploy time.
