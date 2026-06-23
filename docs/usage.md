# ksync user guide

This guide explains how to use ksync, step by step. It uses simple words on purpose, so it is
easy to read for everyone.

## What is ksync?

ksync keeps a local Kubernetes cluster in sync with kustomize directories on your disk.

You run `ksync watch` once. Then you edit your YAML files. Every time you save a file, ksync:

1. **Renders** the app that changed (runs kustomize, including helm charts).
2. **Applies** the result to your cluster (server-side apply).
3. **Prunes** resources that you removed from the files.

This loop is fast (well under one second for a normal app) because ksync is a long-running
process: it keeps a warm connection and cache of the cluster, instead of starting from zero on
every change.

ksync applies changes the same way ArgoCD does, because it is built on
[gitops-engine](https://github.com/argoproj/argo-cd/tree/master/gitops-engine) — the sync
library inside ArgoCD. Hooks, sync waves, prune, and health checks behave like ArgoCD. This
matters when your production runs ArgoCD: what works locally with ksync works the same way in
production.

### When to use it, when not

Use ksync when you develop Kubernetes manifests locally: kustomize overlays, helm values,
config files, dashboards, and so on.

Do not use ksync to deploy to production. It has no server part, no git history, and no UI.
For production, use a GitOps controller (like ArgoCD). ksync is the fast local loop *before*
you push.

## What you need

- A **local Kubernetes cluster** and a kubectl context for it. For example: Docker Desktop
  (context `docker-desktop`), kind, k3d, or minikube.
- The **helm** binary on your `PATH`, but only if your kustomizations use `helmCharts`.
  kustomize itself is built into ksync — you do not need the kustomize binary.
- The **docker** CLI, but only if your apps use `build` (see "Building images from source").
- **Go 1.26+** or **Nix** to build ksync (see below).

## Install

Build from source with Go:

```bash
go build -o ksync ./cmd/ksync
# or, inside this repo:
just build
```

Or build with Nix:

```bash
nix build .#ksync     # binary at ./result/bin/ksync
```

## The config file: `ksync.yaml`

ksync reads one file that lists your apps. By default it looks for `ksync.yaml` in the current
directory. You can choose another file with `-f path/to/file.yaml`.

A full example:

```yaml
# The kubectl contexts ksync is allowed to use. Required, at least one.
# ksync does NOT read your current kubectl context (a host-global setting your
# other shells share). When this lists exactly one concrete context, ksync
# targets it automatically. When it lists several — or a single glob — the
# target is ambiguous, so you must pick one with `ksync sync --context <name>`;
# without it the run is refused rather than guessing a cluster. Either way the
# `--context` you pass must match an entry here, so one config can serve several
# interchangeable dev clusters (here Docker Desktop and a local k3d) while never
# touching the wrong cluster by accident.
#
# Each entry is a shell-style glob (`*`, `?`, `[…]`); a plain name matches
# exactly. A glob covers a family of clusters whose names you don't know up
# front — e.g. per-worktree microVMs `k3s-feature-a`, `k3s-feature-b`, … all
# matched by `k3s-*` (these always need `--context`, having no single concrete
# name). Globs only widen the allowlist, so keep them tight: `*` would match
# every context and defeat the safety gate.
allowedContexts:
  - docker-desktop
  - k3d-dev
  # - k3s-*                      # any per-worktree microVM context

# Optional. Only for clusters whose image store is separate from your docker
# daemon (k3d, kind, a remote cluster). command runs with $KSYNC_IMAGES set to
# the images to load. Leave it out for Docker Desktop. See "Building images".
# imageLoad:
#   command: k3d image import --cluster dev $KSYNC_IMAGES

apps:
  # The smallest possible app: only a path.
  # The app name defaults to the directory name ("shop" here).
  - path: apps/shop

  # An app with every field set.
  - name: api-b                  # explicit name (used in commands, logs, labels)
    path: apps/api-b             # directory that contains kustomization.yaml
    namespace: team-a            # default namespace for this app (see below)
    needs: [db]                  # sync "db" first
    build:                       # build this image from local source (see below)
      - image: example.com/team-a/api-b
        context: ../src/api-b

  - name: db
    path: apps/postgres
    namespace: team-a
```

The fields, one by one:

| Field | Required | Meaning |
|---|---|---|
| `allowedContexts` | yes | The kubectl contexts ksync may target (≥1). ksync ignores your current-context: a single concrete entry is targeted automatically, otherwise (several entries, or a glob like `k3s-*`) you pass `--context`, which must match an entry here. |
| `imageLoad.command` | no | Shell command that makes freshly built images visible to the cluster (k3d/kind/remote). Runs with `$KSYNC_IMAGES` set to the newline-separated refs to load (and `$KSYNC_IMAGE` to the first). Loads never overlap, and images that finish while one runs are coalesced into the next invocation. See "Making built images visible". |
| `buildGroups` | no | Named bulk-build commands several `build` entries can share, so one `docker buildx bake`/compile produces many images. See "Build groups". |
| `apps[].path` | yes | Directory with a kustomization file. Relative paths are resolved from the config file's directory. |
| `apps[].name` | no | Name of the app. Default: the directory name. Used in commands (`ksync sync api-b`), in logs, and as the tracking label value. |
| `apps[].namespace` | no | Default namespace for resources that do not set one (like ArgoCD's `destination.namespace`). ksync creates this namespace if it does not exist. |
| `apps[].needs` | no | Apps that must sync **and become Healthy** before this one starts. ksync checks that there is no cycle. |
| `apps[].build` | no | Images to build from local source code. See "Building images from source" below. |

Unknown fields are an error. This protects you from typos: `need:` instead of `needs:` fails
loudly instead of being ignored.

### About `namespace`

Some rendered resources have no namespace in their metadata. A common example is a ConfigMap
made by `configMapGenerator`. ArgoCD puts such resources into the Application's
`destination.namespace`; ksync does the same with the app's `namespace` field. Without it,
applying a resource that has no namespace fails.

If the namespace does not exist on the cluster, ksync creates it during sync. ksync never
*changes* a namespace that already exists, and it never *deletes* a namespace (see "How
tracking works" below).

Resources that set their own `metadata.namespace` keep it — the default is only for resources
that have none.

## Building images from source

With `build`, ksync closes the whole loop: you save a **source file** (not just a manifest),
and ksync builds the image, updates the manifests, and restarts the pods — like
docker-compose, but on your Kubernetes cluster.

```yaml
apps:
  - path: apps/api-b
    namespace: team-a
    build:
      - image: example.com/team-a/api-b   # the image name your manifests use
        context: ../src/api-b             # the docker build context directory
```

That is all you need for the common case: a directory with a `Dockerfile` in it. The fields:

| Field | Required | Meaning |
|---|---|---|
| `image` | yes | The image name **exactly as your manifests reference it**, without a tag. ksync replaces the tag of every matching image in the rendered output (same rules as kustomize's `images:` field). |
| `name` | no | Short label for this image in progress output and in the watch confirmation prompt (where it names one selectable image). Default: the image's last path segment (`example.com/team-a/api-b` → `api-b`). Must be unique within an app. |
| `context` | yes | The docker build context directory. Relative paths are resolved from the config file's directory. ksync watches it for changes. |
| `dockerfile` | no | Path to the Dockerfile, relative to `context`. Default: `Dockerfile` in the context. |
| `watch` | no | Only these paths (relative to `context`) trigger a rebuild. Useful in monorepos where one big context feeds many images. Default: the whole context. |
| `watchIgnore` | no | Patterns (`.dockerignore` syntax, relative to `context`) excluded from rebuild triggering but **not** from the build context. For build outputs staged inside the context that the Dockerfile `COPY`s — docker must still see them, but their writes must not re-trigger the build. See "Staged build outputs". |
| `command` | no | Replaces `docker build` with your own build command (see below). |
| `group` | no | Build this image as part of a `buildGroups` entry of this name — one bulk command builds it together with the group's other dirty images. Mutually exclusive with `command`/`dockerfile`. See "Build groups". |

### How it works

1. When a watched source file changes, ksync runs `docker build` on the context.
2. The built image gets a tag made from its **content**: `ksync-` plus 12 hex digits of a hash
   of the image's fingerprint — its layer contents plus its runtime config (entrypoint, env,
   …). Same content in, same tag out; an unchanged rebuild yields the identical tag.
3. ksync renders the app and replaces the tag of every matching image — in memory only,
   your manifest files are never modified.
4. The app syncs as usual. Because the tag changed, Kubernetes restarts the pods with the
   new image.

The content-based tag has a nice effect: if you rebuild without changing anything, the tag
is the same, nothing differs, and **no pod restarts**. This is also why ksync needs no state
file — on startup it simply builds everything once (fast, thanks to docker's layer cache)
and lands on the tags that are already deployed.

ksync fingerprints the image it built (its layers and config) rather than trusting the image
ID, because some builders — notably `docker buildx bake` — stamp a fresh build timestamp into
the image config every time, giving unchanged content a new ID. Fingerprinting the layers and
config sidesteps that, so unchanged images keep their tag and their pods do **not** roll, even
through a bake `command` or a build group.

ksync also rewrites `imagePullPolicy: Always` to `IfNotPresent` on the containers running an
image it built. The injected `ksync-…` tag only ever exists locally (and, with `imageLoad`, in
the cluster's store) — never in a registry — so `Always` would make the kubelet try to pull it
and fail with `ErrImagePull`. Only an explicit `Always` is changed; `Never`, `IfNotPresent`,
and an omitted policy are left alone (an omitted policy already means `IfNotPresent` for these
non-`latest` tags). Images ksync does not build are never touched.

Manifest-only edits never run docker: the last built tag is remembered and re-used, so the
fast manifest loop stays fast.

While a build or image-load runs, ksync collapses the tool's output into a single live line — its
latest output line plus elapsed — and shows the full, verbose log **only if the command fails**, so
a normal build stays quiet and a broken one gives you everything.

Each app's work is grouped under its name as a small live **pipeline**, with every stage named so
the icon is never the only signal:

```text
⠹ duo
    ✓ 🔨 Build   rust-core             1.7s
    ✓ 🔨 Build   rust-web              1.8s
    ⠹ 🔨 Build   rust-services (5)     2.3s
    ⠼ 📦 Import  rust-core             6.0s
    ○ 🚢 Deploy
```

The stages run **🔨 Build** → **📦 Import** → **🚢 Deploy** (a group's build shows its member
count). A stage not yet reached is a dim pending row (`○`), so you can see the deploy waiting while
the builds run. An app with **no** builds is just its deploy, shown as a single collapsed line
(`⠼ 🚢 Deploy redis  waiting for health`). Apps sync concurrently, so several pipelines animate at
once; when one finishes it commits its one-line summary to the scrollback and the rest stay pinned
(the block is capped to the terminal height, eliding extra groups with a `… N more` line). In a
pipe or CI there is no live block: each build/import prints a plain finish line and each app its
one-line summary.

### `.dockerignore` decides what triggers a rebuild

A file that `.dockerignore` excludes never enters the image, so ksync also ignores it when
watching. Keep your `.dockerignore` honest (exclude `target/`, `node_modules/`, build
output) and you get correct rebuild triggers for free — including for build commands that
write artifacts back into the context, which would otherwise rebuild forever.
`<dockerfile>.dockerignore` takes precedence over `<context>/.dockerignore`, like BuildKit.

### Staged build outputs (`watchIgnore`)

`.dockerignore` breaks the rebuild-forever loop only for outputs the image does **not** need.
Some flows stage a build output *inside* the context because the Dockerfile `COPY`s it — a
host-compiled binary placed in `apps/<svc>/.zigbuild/` for a thin Dockerfile, say. That path
cannot be `.dockerignore`d (docker would drop it from the `COPY`), yet writing it must not
re-trigger the build that produced it. List such paths in `watchIgnore`: they stay in the
build context but are excluded from change detection.

```yaml
build:
  - image: example.com/team-a/api-b
    context: .
    watch: [apps/api-b, lib]
    group: native
    watchIgnore: ["**/.zigbuild"] # staged binaries the thin Dockerfile COPYs
```

### Custom build commands

If `docker build` is not enough (multi-target bake files, compile-on-host flows, build
args), set `command`. ksync runs it with `sh -c` inside the context directory. Your command
must leave the finished image in the local docker daemon under the name ksync passes in the
`KSYNC_IMAGE` environment variable:

```yaml
build:
  - image: example.com/team-a/api-b
    context: ../..
    watch: [apps/api-b, lib]
    command: docker buildx bake --load --set "api-b.tags=$KSYNC_IMAGE" api-b
```

Quote `$KSYNC_IMAGE` with **double** quotes, not single quotes: `sh -c` does not expand a
variable inside single quotes, so `--set 'api-b.tags=$KSYNC_IMAGE'` passes the literal text
`$KSYNC_IMAGE` to the build and fails with "invalid reference format". Use double quotes around
any argument that contains it, and single quotes only around arguments that must *not* expand
(e.g. a bake `--set '*.platform=…'`, where `*` would otherwise glob).

You do not need to make your build reproducible for content addressing to work: ksync tags by
the built image's layers and runtime config, not its image ID, so a `docker buildx bake` (which
restamps the image ID every build) still produces a stable tag for unchanged content. Passing
`--provenance=false` is still tidy — it drops an attestation manifest you do not need locally —
but it is no longer required to avoid needless pod restarts.

### Build groups: building many images with one command

When several images come out of **one** build — a multi-target `docker buildx bake`, a host
compile that produces many binaries — running a separate `command` per image is wasteful: the
shared work (a common base image, one compiler pass) repeats, the builds run one after another,
and each image imports into the cluster separately. A **build group** hands the whole set to a
single command instead.

Declare the group at the top level and point the member `build` entries at it with `group`:

```yaml
buildGroups:
  - name: services
    # Builds every image the batch asks for. $KSYNC_IMAGES is the newline-separated
    # list of <image>:ksync-build temp tags to produce — only the images that
    # actually changed, which may be just one.
    command: |
      sets=""; targets=""
      for ref in $KSYNC_IMAGES; do          # ref = ghcr.io/team-a/api-b:ksync-build
        t=${ref%:*}; t=${t##*/}             # -> api-b  (the bake target name)
        sets="$sets --set ${t}.tags=${ref}"
        targets="$targets $t"
      done
      docker buildx bake --load --set '*.attest=' $sets $targets

apps:
  - name: services
    path: manifests/services
    build:
      - image: ghcr.io/team-a/api-b          # no command/dockerfile: the group builds it
        context: ../..
        watch: [services/api-b, lib]
        group: services
      - image: ghcr.io/team-a/api-c
        context: ../..
        watch: [services/api-c, lib]
        group: services
```

How it behaves:

- A grouped entry sets **neither `command` nor `dockerfile`** — the group's command builds it.
  It keeps `context` and `watch`, which still decide what dirties it.
- ksync builds only the **dirty subset**: edit one service and the command runs with a single
  ref in `$KSYNC_IMAGES`; edit shared code and all the dirty members build in one invocation.
- After the command finishes, ksync content-tags each image exactly as for a single build, so
  the no-rollout-on-unchanged guarantee still holds **per image**: a bulk bake of six targets
  where only two changed rolls only those two pods.
- All members of a group must share one `context` (the command's working directory).
- The command must leave each requested image tagged `<image>:ksync-build` in the daemon —
  the same temp-tag contract as a single `command` build, just for many images at once.
- The command runs under `sh -c`. `$KSYNC_IMAGES` is newline-separated specifically so an
  unquoted `for ref in $KSYNC_IMAGES` word-splits into one ref per iteration. Leave it unquoted
  in the loop (the refs never contain spaces).

A group's command is arbitrary shell, so it also fits the **host-compile then thin image**
shape: pre-build the binaries on the host (one compiler pass), then `bake` thin Dockerfiles that
just `COPY` them in. Only the dirty subset is asked for, so editing one service compiles and
bakes only that one:

```yaml
buildGroups:
  - name: services
    command: |
      names=""; sets=""
      for ref in $KSYNC_IMAGES; do          # ref = ghcr.io/team-a/svc-x:ksync-build
        n=${ref%:*}; n=${n##*/}             # -> svc-x  (service / bake target name)
        names="$names $n"
        sets="$sets --set ${n}.tags=${ref}"
      done
      just prebuild $names                  # host compile (e.g. cargo zigbuild) -> ./.build/…
      docker buildx bake --load --set '*.attest=' $sets $names
```

ksync content-tags each thin image by its fingerprint (the copied binary's layer), so editing
one service's source rebuilds its binary, changes its image, and rolls only that pod — the rest
keep their tags. (ksync renders kustomize; if your services are deployed another way — a raw
helmfile, say — give each a small kustomization that inflates its chart via `helmCharts:` so
ksync can render and tag-inject it.)

### Using a pre-built image instead of building (`--image` / `KSYNC_IMAGE_OVERRIDES`)

Sometimes the image already exists and you do not want ksync to build it: a CI artifact, a
`docker pull` of a registry tag, a pinned digest, or a wrapper script that resolves a ref per
service its own way. Give ksync the ref and it deploys that instead of building from source — for
that image only; the app's other builds are unaffected.

Two equivalent inputs, accepted by `sync` (not `watch` — see below):

```bash
# Repeatable flag — IMAGE is the build entry's image:, REF is the ref to deploy.
ksync sync --image ghcr.io/team-a/api-b=ghcr.io/team-a/api-b:ci-1234 \
           --image ghcr.io/team-a/ui-b=ghcr.io/team-a/ui-b@sha256:abc…

# Env var — whitespace/newline-separated IMAGE=REF tokens (handy for a script).
export KSYNC_IMAGE_OVERRIDES="
  ghcr.io/team-a/api-b=ghcr.io/team-a/api-b:ci-1234
  ghcr.io/team-a/ui-b=ghcr.io/team-a/ui-b@sha256:abc…
"
ksync sync
```

- `IMAGE` is the `build` entry's `image:` exactly (the bare name; it is unique across the config).
- `REF` is a bare tag (`ci-1234`), `:tag`, a full `name:tag`, a digest (`@sha256:…`), or
  `name@digest`. A name that differs from `IMAGE` redirects the registry/repo; otherwise only the
  tag/digest changes. A flag beats the env var for the same image.
- An overridden image is **not built**. Because the build is skipped, the `imageLoad` step is
  skipped too — **making the supplied image visible to the cluster is the supplier's job** (it is
  already a registry image the cluster can pull, or you loaded it).
- An override naming an image no app builds is ignored with a note, so a wrapper can hand ksync its
  full set of resolved refs without tracking which ones ksync builds.

**`watch` rejects overrides.** `watch` exists to rebuild the stack from source, which an override
contradicts, so it does not offer `--image` and **fails fast** if `KSYNC_IMAGE_OVERRIDES` is set
(rather than silently ignoring it). Use `ksync sync` to deploy a pre-built image. See ADR
20260623-watch-rejects-image-overrides.

This is what lets a wrapper own image resolution and use ksync purely as the deploy engine: resolve
every ref (build/pull/pin), make them cluster-visible, then `ksync sync` with the overrides. See
ADR 20260616-image-override.

### Making built images visible to the cluster

ksync builds into your **local docker daemon**. Docker Desktop's Kubernetes runs pods straight
from there, so nothing else is needed. But k3d and kind keep their own image store inside the
node, and a remote cluster cannot see your daemon at all — a freshly built `ksync-<hash>` tag
never reaches them, and the pod fails to start.

For those, set `imageLoad.command`: a command ksync runs with `$KSYNC_IMAGES` set to the
newline-separated built references (`<image>:ksync-<hash>`) to load, and `$KSYNC_IMAGE` to the
first of them. It is the mirror image of a build `command` — a build *produces* the refs,
`imageLoad` *consumes* them.

```yaml
# k3d (imports every image of the batch in one call):
imageLoad:
  command: k3d image import --cluster dev $KSYNC_IMAGES
# kind:
imageLoad:
  command: kind load docker-image --name dev $KSYNC_IMAGES
# k3s (save the batch and import it into containerd's k8s.io namespace in one go):
imageLoad:
  command: docker save $KSYNC_IMAGES | k3s ctr -n k8s.io images import -
# remote cluster that pulls from a registry your manifests point at:
imageLoad:
  command: for i in $KSYNC_IMAGES; do docker push "$i"; done
```

Use `$KSYNC_IMAGES` (plural) so several images import together; `$KSYNC_IMAGE` (the first ref)
still works for the single-image case. `$KSYNC_IMAGES` is newline-separated, so an unquoted use
word-splits into one argument per image. A command that cannot take many images at once should
loop over `$KSYNC_IMAGES` itself (the `docker push` line above).

**One config, several clusters — `$KSYNC_CONTEXT`.** When `allowedContexts` lists more than one
cluster, the load step must do different things per target: nothing for a shared-daemon Docker
Desktop, an import for a separate-store cluster. ksync exports the **selected** context as
`$KSYNC_CONTEXT` to the `imageLoad` command (and to build commands), so one config branches on it
instead of needing a per-cluster file:

```yaml
allowedContexts: [docker-desktop, k3d-dev]
imageLoad:
  command: |
    [ "$KSYNC_CONTEXT" = "docker-desktop" ] && exit 0   # shared daemon — nothing to do
    k3d image import --cluster dev $KSYNC_IMAGES         # separate store — import
```

With two entries the target is ambiguous, so each run names one: `ksync sync --context
docker-desktop` skips the import, and `ksync sync --context k3d-dev` runs it — same config, same
images.

ksync stays out of the way here on purpose: it has no built-in idea of "k3d" or "kind", so the
`command` above is exactly what runs — and any other tool or transport works the same way without
waiting for a ksync release. The load runs only when an image is actually (re)built, so the
fast manifest-only loop never pays for it. With k3d/kind, set the pods' `imagePullPolicy` to
`Never` or `IfNotPresent` so the kubelet uses the imported image instead of trying to pull it.

**Concurrency: serialized and coalesced.** ksync builds apps in parallel, but the load step never
overlaps — two `imageLoad` commands never run against one cluster at once. This is because some
importers corrupt under concurrency: `k3d image import` stages every import through one shared
per-cluster "tools" node and a tarball named only to the second in a shared volume, then deletes
them on cleanup, so two imports overlapping in time clobber each other and silently import nothing,
leaving pods in `ErrImageNeverPull` behind a green ✓. Serializing is safe for every loader, so you
write no flag.

To keep that from being slow, ksync **coalesces**: while one load runs, images from other builds
that finish in the meantime queue up, and the next load carries the whole queue in its
`$KSYNC_IMAGES` — one `k3d image import a b c` instead of three separate imports. This matters
because the per-call cost dominates: `k3d image import` (and `kind load`) transfer a *whole image
tarball* and spin up a tools node per call — a few seconds regardless of image count (measured
~3.3s for a ~120 MB image; `k3d image import --mode direct` shaves it to ~2.8s), re-sending every
layer because a tarball has no notion of "already present". Editing one service rebuilds and
imports just that one image — fast. A cold `watch`/`sync` that builds many images amortizes the
fixed cost by batching whatever has piled up into each import, so first convergence is faster than
one-import-per-image would be; steady-state single-edit loops have nothing to batch and are
unaffected.

#### k3d fast-path: push to a registry instead of importing

`k3d image import` is the zero-setup default, but the tarball transfer dominates the loop once the
build itself is incremental. If you want a sub-second load, give the cluster a local registry and
**push** instead — a registry only transfers the layers it does not already have, so an incremental
rebuild moves one layer:

```yaml
# imageLoad: retag each built ref to the local registry and push it.
imageLoad:
  command: |
    for ref in $KSYNC_IMAGES; do
      docker tag "$ref" "localhost:5111/${ref#*/}"
      docker push "localhost:5111/${ref#*/}"
    done
```

Measured against the same image as above: **~0.8s** to push (cold *and* warm — the layers are
local), versus ~3.3s to import. On a one-service edit that brings the cluster-load step from the
biggest cost after the build down to noise.

Two pieces of cluster setup make it transparent — the pod keeps pulling its original
`ghcr.io/...` (or any registry) name, no manifest rewrite:

- Create k3d with a registry it can pull from: `k3d cluster create … --registry-use <name>:5111`
  (or `k3d registry create` + `--registry-use`).
- Add a **mirror** so the manifests' registry resolves to that local one, via
  `k3d cluster create … --registry-config <file>` where the file maps the host:

  ```yaml
  mirrors:
    "ghcr.io":                          # whatever host your image names use
      endpoint:
        - "http://<registry-name>:5000" # the registry's in-cluster address
  ```

  Push to `localhost:5111/<path>` (the registry's host-side port); the kubelet, pulling
  `ghcr.io/<path>`, is redirected to `http://<registry-name>:5000/<path>` — the same blob.

Use `imagePullPolicy: IfNotPresent` (not `Never`) so the kubelet pulls each new `ksync-<hash>` tag
from the mirror the first time it sees it; unchanged tags stay cached on the node. The pull of one
fresh ~28 MB layer from a local registry is ~250 ms. The result is a k3d loop whose only real costs
are the build and the pod roll — the cluster transport is no longer one of them.

### Other limits, in plain words

- Containers that set `imagePullPolicy: Always` cannot use locally built images — the
  kubelet would try to pull the ksync tag from a registry. Most charts let you change the
  policy; the Kubernetes default (`IfNotPresent` for non-`latest` tags) is fine.
- One image name can have only one build definition across all apps.

## Commands

Every command accepts `-f <file>` to choose the config file, and an optional list of app names.
No app names means **all** apps.

### `ksync watch` — the main loop

```bash
ksync watch                 # watch all apps
ksync watch api-b shop      # watch only these apps
```

What it does:

1. On start, it builds every `build` image and syncs every watched app once, so the cluster
   matches your files.
2. Then it watches the app directories for file changes. It also watches files *outside* the
   app directory that the kustomization points to: a shared `chartHome`, values files,
   `resources:` entries, and so on — and the source directories of `build` entries.
3. When you save a file, ksync waits a short quiet period (debounce, default 200ms), so one
   "save all" in your editor becomes one sync, not ten — then **asks what to rebuild** instead
   of acting on its own.
4. The confirmation prompt lists each changed image (🔨) and each manifest-only app (🚢) and lets
   you choose with the keyboard: **Build all** (the default — just press Enter), **Select which
   to build** (then ↑/↓ to move, Space to toggle each one, `a` for all, Enter to confirm), or
   **Skip**. ksync rebuilds or redeploys nothing until you choose, so a mid-edit save costs nothing.
5. Only the chosen images are rebuilt and their apps re-applied (a chosen manifest-only app is just
   re-applied, no rebuild). Unchosen changes stay pending and are offered again the next time the
   loop is idle, until you Skip them. If two apps share a chart directory and you edit the chart,
   both appear in the prompt. While a build or deploy is in flight, new saves accumulate quietly and
   the prompt reappears once it finishes.
6. If a sync fails (for example, the YAML is broken half-way through your edit), ksync prints
   the error, keeps running, and retries with growing wait times. The next file save resets
   the retry and syncs immediately again.

Builds run **ahead of the `needs` order**: an image is local (build + load into the cluster), so it
starts the moment its source changes, regardless of which apps it depends on — only the *deploy*
waits for dependencies to be Healthy. On a cold start of a deep stack this overlaps every app's build
with the dependency chain that precedes it, so a dependent's image is already built by the time its
turn to deploy arrives. The deploy still never applies an image that has not finished building.

Stop it with Ctrl-C; inside the prompt, `q` or Ctrl-C also quits. The confirmation prompt appears
only on an interactive terminal — under a pipe, a redirect, or a process manager (or with `-auto`)
ksync rebuilds automatically on every change, with no prompt. There is no separate "resync
everything" key: re-save any watched file to re-open the prompt, or run `ksync sync` for a one-shot
re-apply of everything.

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-auto` | `false` | Rebuild and redeploy automatically on every change, skipping the confirmation prompt (the pre-prompt behavior). Always on when stdin is not an interactive terminal. |
| `-debounce` | `200ms` | Quiet period after the last change before prompting. |
| `-max-parallel` | CPU cores | How many apps may **build** at once and, separately, how many may **deploy** (render + sync) at once — builds and deploys have independent budgets of this size, since builds are CPU/IO-heavy while deploys mostly wait on health. Also bounds, within one app, how many of its independent images build at once. `0` removes the limit. |
| `-prune` | `true` | Delete tracked resources that you removed from the files. |
| `-timeout` | `5m` | Max time to wait for one app to become healthy before giving up and retrying. `0` disables the limit. |
| `-v` | `false` | Verbose: also log every detected file change. |

Each sync commits the same summary `ksync sync` does: a build-less app a single
`✓ 🚢 api-b  2 applied  0.9s` line — the 🚢 icon and the `applied` count already say what happened,
so it carries no redundant "Deploy" word — and a build app its whole 🔨 Build → 📦 Import →
🚢 Deploy stage tree, each row keeping its own time so a slow build is still visible afterward. The
`✓`/`⚠`/`✗` status symbol means applied & healthy / applied but a resource is degraded / a sync task
failed; the deploy time is that stage's own (apply + health gate). `applied` is what actually changed
(a no-op edit reads `0 applied`); `pruned`, `failed`, and `degraded` show only when nonzero
(`degraded` is the post-sync health check described under `ksync sync` below).

A whole-stack `watch` frames its **startup convergence** exactly like a `sync` run — a titled
**Plan**, a live **Summary** footer pinned to the bottom while the apps come up, and the committed
Summary block once every app has synced once (see the `ksync sync` example below). A stuck app keeps
the footer open rather than committing a false "done". Each later rebuild batch is framed the same
way: once it settles, ksync commits a fresh **Summary** for just that batch (the apps it touched and
how long it took) so you see the result of every edit, not only the first convergence. After every
settle — the initial convergence and each batch — ksync logs a `finished, watching for changes` line,
the cue that the loop is idle and ready for your next save. A single-app `watch` (e.g. `watch duo`)
skips the Plan/Summary framing and streams the one line like a single-app sync, but still logs the
watching line when it settles.

### `ksync sync` — one-time sync

```bash
ksync sync                  # sync all apps once, in needs-order
ksync sync api-b
```

Renders and applies once, then exits. Useful for scripts, or to converge the cluster before
starting `watch`. Independent apps build, render, and apply **concurrently** (up to
`-max-parallel`, default the host's CPU-core count, `0` for no limit); the `needs` DAG still holds a dependent app until the apps it
needs have finished — the same model `watch` uses, so a one-time sync is never slower than the
loop's startup pass. Apps with `build` entries build their images first, so what gets applied
always points at images that exist — and an app's own independent images (its ungrouped entries and
each build group) build concurrently too, also up to `-max-parallel`, so a multi-image app is not
bottlenecked on building one at a time.

An app **finishes only once its resources are Healthy**, not merely applied: after applying, ksync
waits for every workload to reach Ready (Deployments rolled out, StatefulSets up, Jobs complete),
up to `-timeout`. This is what makes `needs` meaningful — a dependent does not start against a
database whose pod is still pulling its image; it waits until that database is actually serving.
While an app is in this wait its **🚢 Deploy** row reads `waiting for health  N not ready` (on a
terminal), then resolves to its committed form once Ready — one apply line for a build-less app, the
frozen stage tree for a build app. An app that cannot become Healthy (e.g. a workload
crash-looping on a missing external prerequisite) blocks until `-timeout` and then fails, naming the
resources still not healthy — so a broken deploy surfaces instead of passing as `✓ applied`. An
already-healthy re-sync returns immediately (the wait finds nothing pending).

**Hooks and re-running them.** A no-change sync skips hooks, so a PostSync Job that already ran is
not re-run — the fast path stays a quick `0 applied`. Two cases override that: a hook whose Job is
currently **failed** (Degraded — it hit its backoff limit) is re-run on the next sync, so a
transient failure (an upstream blip while a provisioning Job ran) self-heals rather than leaving a
dependent stuck on a side-effect that never happened; and `--force` re-runs **every** hook even
when nothing changed (ArgoCD's manual-sync semantics). Use `--force` to re-apply a release whose
source did not change, or to recover a hook that failed and whose Job has since been cleaned up
(absent, so the automatic failed-hook re-run cannot see it):

```bash
ksync sync --force            # re-run all hooks for the synced apps, diff or no diff
ksync sync --force sistema    # …scoped to one app and what it needs
```

If an app's resources reference namespaces it does not own (a chart that fans RBAC out across other
apps' namespaces, say), ksync creates those namespaces if missing — bare and untracked, so prune
never touches them and the app that owns one adopts it on its own sync. The common single-namespace
app is unaffected.

A build-less app commits a one-line summary led by the **🚢** icon (the app is the subject, so no
"Deploy" word is needed); a build app commits its whole stage tree, each 🔨 Build / 📦 Import / 🚢
deploy row keeping its time. The status symbol tells you the outcome at a glance — `✓` applied and
healthy, `⚠` applied but a resource is broken at runtime, `✗` a sync task failed. In a whole-stack
run the app name is padded to the widest so the `applied` (and, when the counts read alike, the
duration) column lines up across the apps:

```text
✓ 🚢 api-b  3 applied, 1 pruned
  ⚠ apps/Deployment/shop/web: Degraded — progress deadline exceeded
⚠ 🚢 shop   0 applied, 1 degraded
duo
    ✓ 🔨 rust-services (3)  1m20s   ← a build app keeps its per-stage times
    ✓ 📦 rust-services (3)  8.0s
    ✓ 🚢 deploy             21 applied  1m52s
```

The `⚠` is a post-sync health snapshot: a resource that applied cleanly but is **Degraded** (a
crash-looping or failed workload, a Deployment whose rollout gave up). It is read from the warm
cache, so it adds no cluster round-trips, and it only flags genuinely-broken resources — a rollout
still in flight is *Progressing*, not Degraded, so a healthy edit never trips a false warning. (The
flip side: a wedged StatefulSet carries no progress deadline and stays Progressing, so it is not
caught.)

A whole-stack sync (more than one app) frames the run: a titled **Plan** up front shows the scope,
a live **Summary** block stays pinned to the bottom and updates as apps finish (the per-app lines
scroll above it), and the same block is committed when the run completes — so the result is legible
at a glance without scanning every line:

```text
Plan
  16 apps → docker-desktop
  postgres redis traefik … duo sistema

✓ 🚢 redis     0 applied  0.4s   ← finished build-less apps commit one line to the scrollback,
✓ 🚢 postgres  0 applied  0.6s     the name padded so the columns line up

⠹ duo                              ← in-flight apps show their live pipeline,
    ⠹ 🔨 Build   rust-services (3)  2.3s    grouped under the pinned, updating block
    ○ 🚢 Deploy                            (a build app commits this whole tree, frozen, when it finishes)

Summary
      Apps  12/16 synced
  Duration  0.8s

… and on completion the block is committed with the final tally:

Summary
      Apps  16 synced
  Degraded  1  shop
  Duration  1.6s
```

The plan and the pinned/committed Summary appear only for a multi-app run; a single-app sync stays
one line. In a pipe or CI the live block is dropped (no terminal to pin it to), but the plan,
per-app lines, and final Summary still print.

For a large stack (many apps), the default already matches your CPU-core count. Rendering — each
app's helm inflation — is CPU-bound and saturates there, so a purely render-bound run gains nothing
from going higher. But apps also spend time building images (docker) and waiting for health, both
mostly idle for the CPU; when those dominate, `-max-parallel 0` (no limit) or a value above your
core count can still shorten the run.

It takes the same `-timeout` (default `5m`), `-max-parallel`, and `-v` flags as `watch`, plus
`--force` (re-run hooks even with no diff, above) and `--image`/`KSYNC_IMAGE_OVERRIDES` (deploy a
pre-built image instead of building it — see "Using a pre-built image"). The timeout matters
most here: a one-time sync waits for the app to become healthy, so without it a pod stuck in
`ErrImagePull` would hang `ksync sync` forever. On timeout the sync fails and names the
resources that never became healthy, and prints a short diagnostic dump — each unhealthy
resource's recent events, plus the related pods' container state and current/previous log tails —
so you can see what wedged it without reaching for `kubectl`. The same dump prints in `watch` when
a deploy times out. While the health gate waits, the live deploy line names the not-ready
resources so you see what it is blocked on.

### `ksync render` — print the YAML

```bash
ksync render api-b          # render one app to stdout
ksync render > all.yaml     # render every app
```

Renders the kustomization (including helm chart inflation) and prints the result. It does not
run docker: the output shows the manifests as written, without locally built dev tags. Use it
to check what ksync *would* apply, or to debug a kustomization.

By default ksync renders helm charts **against your cluster** (`--dry-run=server`), so a chart's
`lookup` calls — reading a live Service, ConfigMap, etc. at template time — resolve. This is the
common "helm but not GitOps" pattern (e.g. resolving a Service's ClusterIP into a pod
`hostAliases`); a plain offline `helm template` returns empty for those and a chart that `fail`s
on a missing lookup will not render at all. ksync can do this because it only ever runs against
one explicitly allowlisted local context. The render output is otherwise unchanged — for a chart
that uses no `lookup`, it is byte-identical to offline.

Pass `--offline-render` (on `render`, `sync`, and `watch`) to render with a plain offline
`helm template` instead — useful for a quick `ksync render` with no cluster reachable, or to
avoid the small per-render cluster round-trip. Offline, the output is the same bytes that
`kustomize build --enable-helm --load-restrictor LoadRestrictionsNone <dir>` produces (verified
by tests); charts that require `lookup` will not render.

`render` and `images` render the selected apps **concurrently** (`-max-parallel`, default the
host's CPU cores; `0` runs one worker per app). Each chart release inflates with a live-cluster
`helm` dry-run, so rendering is I/O-bound and apps overlap; the wall-clock is bounded by the
slowest single app rather than the sum (rendering a many-app config one at a time is otherwise the
dominant cost). The output is unaffected — `render` still emits apps in config order, `images`
is still a sorted set.

### `ksync images` — list the images the apps deploy

```bash
ksync images                # every image ksync.yaml deploys, one per line
ksync images shop           # just one app's images
ksync images --live         # also include operator-derived images (see below)
```

Renders the selected apps and prints the **canonical** references of the container images they
deploy — sorted, deduplicated, one per line on stdout. "Canonical" means the same fully-qualified
form a container runtime stores: `redis:7` becomes `docker.io/library/redis:7`, an untagged image
gets an explicit `:latest`. So the output can be matched against a cluster's image store by plain
string equality — which is what makes it useful for **scoping an image cache or a pre-pull step to
exactly what ksync deploys**, rather than to whatever a node happens to have accumulated.

Images ksync builds locally (any `build:` entry's image) are **excluded**: those are
content-addressed dev tags that live only in the local store and are never pulled, so caching them
is pointless.

A plain `ksync images` lists only what the manifests literally name. Images a controller derives
at runtime are not in the rendered YAML — for example an ECK `Elasticsearch` whose data image
comes from `spec.version`, not an `image:` field. Add **`--live`** to also read the images of
running pods in the apps' namespaces, which captures those. `--live` needs a reachable cluster
(it reuses the same `--context` rules as the other commands); the plain form needs none.

### `ksync destroy` — delete what ksync created

```bash
ksync destroy               # refuses, and lists the apps it would delete
ksync destroy -yes          # actually deletes
ksync destroy -yes shop     # delete only one app's resources
```

Deletes every resource that ksync tracks for the selected apps — and nothing else. Apps go
down in reverse dependency order (dependents first). Namespaces are never deleted, even
namespaces that ksync created. Like `sync`, it takes `-timeout` (default `5m`) to bound how
long it waits for resources to finish deleting.

## Output and logs

ksync writes its status (build, sync, watch events) to **stderr**, so `ksync render`'s manifest
output on **stdout** stays clean and pipeable. The Kubernetes client and the sync engine are
silenced down to genuine errors — only ksync's own events and real failures are shown.

Output is colored when stderr is an interactive terminal. Set `NO_COLOR` (any value) to disable
color; it is off automatically when the output is piped or redirected. Pass `-v` to `sync` or
`watch` to also see each detected file change.

### `ksync diff` — not implemented yet

Planned: show the difference between your local files and the live cluster, without applying.

## How tracking works (and why prune is safe)

Every resource ksync applies gets a label:

```yaml
labels:
  ksync.dev/app: api-b      # the app name from ksync.yaml
```

When ksync prunes (or destroys), it only ever deletes resources that carry this label with the
right app name. Resources made by anyone else — your colleagues' tools, controllers, the
cluster itself — are invisible to prune. The namespaces ksync auto-creates do *not* get the
label, so they are never pruned.

Apply uses **server-side apply** with the field manager `ksync`. This is the same apply method
production ArgoCD setups use, and it avoids the size limits of the old client-side apply
annotation (big generated ConfigMaps are fine).

## Hooks and sync order

ksync follows **ArgoCD's** rules, not Helm's. The important differences:

- Hooks are read from annotations on the rendered objects: `argocd.argoproj.io/hook` first,
  falling back to `helm.sh/hook`.
- A `post-install` or `post-upgrade` hook becomes **PostSync** and runs on **every** sync,
  after the main resources are healthy — not only on the first install.
- `helm.sh/hook-weight` is used as the sync wave when `argocd.argoproj.io/sync-wave` is not
  set.
- `helm.sh/hook-delete-policy` works as in ArgoCD; the default is `BeforeHookCreation`
  (the old hook object is deleted right before the new one is created).

If your charts rely on hooks behaving exactly like `helm install`, check this list first.

## Safety model

- ksync talks only to a context in `ksync.yaml`'s `allowedContexts`, and it ignores your kubeconfig
  current-context entirely (that host-global setting belongs to your other shells, not to ksync).
  A single concrete entry is targeted automatically; with several entries or a glob you pass
  `--context <name>`, which must itself match an entry, and a run that cannot resolve a single
  target is refused rather than guessing. So a config checked into a repo can never point a
  teammate's ksync at an unlisted cluster (production, a colleague's cluster), whatever their
  current-context happens to select. The allowlist is what lets one config serve several
  interchangeable dev clusters. Entries are shell-style globs (`k3s-*`), which only ever widen the
  set — keep them tight, since `*` matches every context and so disables this gate.
- Prune and destroy only touch resources labeled with `ksync.dev/app`.
- `destroy` requires `-yes`.

## Troubleshooting

**`an empty namespace may not be set when a resource name is provided`**
A rendered resource has no `metadata.namespace`. Set `namespace:` on the app in `ksync.yaml`.

**`context "X" does not exist`**
The context in `ksync.yaml` is not in your kubeconfig. Check `kubectl config get-contexts`.

**`no kustomization file in <dir>`**
The app `path` must point at a directory that contains `kustomization.yaml` (or
`kustomization.yml`, or `Kustomization`).

**Helm chart errors during render**
ksync runs the `helm` binary for `helmCharts` inflation. Make sure `helm` is installed and the
chart's `values.yaml` exists.

**A change is not picked up by `watch`**
ksync watches the app directory and everything the kustomization references (chart home,
values files, resources). It ignores `.git` directories and editor temporary files. If you
reference a file in some other way (for example through a symlink target outside all watched
directories), a manual `ksync sync` always works.

**First install of a chart that ships CRDs fails for the custom resources**
When one app contains both CRDs and resources *of* those CRDs (many operator charts do),
the very first sync can fail for the custom resources with `the server could not find the
requested resource`: the cluster needs a moment to activate a new CRD, and the resources
are applied in the same pass. This heals by itself: `ksync watch` retries and converges on
the next attempt, and a second `ksync sync` completes the install.

**The same one or two resources are re-applied on every sync**
ksync only applies resources whose rendered content differs from the cluster. Some charts
generate fresh content on every render — the common case is a self-signed certificate for
an admission webhook, made new on every `helm template`. ksync sees a real difference and
applies it, the same way ArgoCD would. It is harmless noise; configuring the chart to use a
stable certificate (for example cert-manager) makes it go away.

**First sync after start is slow**
On start, ksync lists the cluster's resources once to build its cache (a few seconds on a
local cluster with many CRDs). Every sync after that uses the warm cache and is fast. Keep
`watch` running instead of restarting it.

**Pods of a built image show `ErrImagePull` or `ImagePullBackOff`**
The kubelet tried to pull the `ksync-…` tag from a registry, which does not have it. ksync
already rewrites an explicit `imagePullPolicy: Always` to `IfNotPresent` for images it builds,
so the usual remaining causes are: the image is referenced by a manifest with **no matching
`build` entry**, so ksync never built or imported it (add the `build` entry — this is the most
common mistake); or the cluster keeps a **separate image store** and `imageLoad` is not set, so
the built image was never imported (see "Making built images visible"); or the cluster simply
cannot see your docker daemon's images (use Docker Desktop Kubernetes, or set `imageLoad`).

**`sync of "X" timed out; still not healthy: …`**
A sync waits for the app's resources to become healthy (so dependent apps and PostSync hooks
see a ready dependency). If something never becomes healthy — a pod stuck in `ErrImagePull`,
a crash loop — the sync gives up after `-timeout` (default `5m`) and lists the resources it
was waiting on. Fix the named resource (see the `ErrImagePull` entry above), then sync again.
Raise `-timeout` for genuinely slow rollouts, or set it to `0` to wait forever.

**Pods restart on every rebuild, even when nothing changed**
The image ID changes on every build. The usual cause is docker's provenance attestation,
which embeds build timestamps. ksync's own `docker build` disables it; if you use
`command`, add `--provenance=false` to your docker invocation.

**A source edit does not trigger a rebuild**
ksync watches the build `context` minus what `.dockerignore` excludes (an excluded file
cannot change the image), and only the listed paths when `watch:` is set. Check whether the
file falls under an excluded pattern or outside the watched paths. A manual `ksync sync`
always builds.
