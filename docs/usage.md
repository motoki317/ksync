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
# The kubectl context ksync is allowed to use. Required.
# ksync ONLY ever talks to this context. It never uses your current kubectl
# context, so it can never touch the wrong cluster by accident.
context: docker-desktop

# Optional. Only for clusters whose image store is separate from your docker
# daemon (k3d, kind, a remote cluster). Runs once per freshly built image with
# $KSYNC_IMAGE set. Leave it out for Docker Desktop. See "Building images".
# imageLoad: k3d image import --cluster dev $KSYNC_IMAGE

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
| `context` | yes | The only kubectl context ksync will use. |
| `imageLoad` | no | Shell command that makes freshly built images visible to the cluster (k3d/kind/remote). Runs once per build batch with `$KSYNC_IMAGES` set (and `$KSYNC_IMAGE` to the first). See "Making built images visible". |
| `buildGroups` | no | Named bulk-build commands several `build` entries can share, so one `docker buildx bake`/compile produces many images. See "Build groups". |
| `apps[].path` | yes | Directory with a kustomization file. Relative paths are resolved from the config file's directory. |
| `apps[].name` | no | Name of the app. Default: the directory name. Used in commands (`ksync sync api-b`), in logs, and as the tracking label value. |
| `apps[].namespace` | no | Default namespace for resources that do not set one (like ArgoCD's `destination.namespace`). ksync creates this namespace if it does not exist. |
| `apps[].needs` | no | Apps that must sync before this one. ksync checks that there is no cycle. |
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
| `context` | yes | The docker build context directory. Relative paths are resolved from the config file's directory. ksync watches it for changes. |
| `dockerfile` | no | Path to the Dockerfile, relative to `context`. Default: `Dockerfile` in the context. |
| `watch` | no | Only these paths (relative to `context`) trigger a rebuild. Useful in monorepos where one big context feeds many images. Default: the whole context. |
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

While a build (or image import) runs, ksync collapses the tool's output into a single live
line — `⠹ build api-b  <latest output line>  12s` — and prints a `✓ build api-b (12s)` when it
finishes. The full, verbose build log is shown **only if the command fails**, so a normal
build stays quiet and a broken one gives you everything. (In a pipe or CI, the spinner is
replaced by plain start/finish lines.)

### `.dockerignore` decides what triggers a rebuild

A file that `.dockerignore` excludes never enters the image, so ksync also ignores it when
watching. Keep your `.dockerignore` honest (exclude `target/`, `node_modules/`, build
output) and you get correct rebuild triggers for free — including for build commands that
write artifacts back into the context, which would otherwise rebuild forever.
`<dockerfile>.dockerignore` takes precedence over `<context>/.dockerignore`, like BuildKit.

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

### Making built images visible to the cluster

ksync builds into your **local docker daemon**. Docker Desktop's Kubernetes runs pods straight
from there, so nothing else is needed. But k3d and kind keep their own image store inside the
node, and a remote cluster cannot see your daemon at all — a freshly built `ksync-<hash>` tag
never reaches them, and the pod fails to start.

For those, set `imageLoad`: a command ksync runs once per build batch, with `$KSYNC_IMAGES` set
to the newline-separated built references (`<image>:ksync-<hash>`) and `$KSYNC_IMAGE` to the
first of them. It is the mirror image of a build `command` — a build *produces* the refs,
`imageLoad` *consumes* them.

```yaml
# k3d (imports every image of the batch in one call):
imageLoad: k3d image import --cluster dev $KSYNC_IMAGES
# kind:
imageLoad: kind load docker-image --name dev $KSYNC_IMAGES
# remote cluster that pulls from a registry your manifests point at:
imageLoad: for i in $KSYNC_IMAGES; do docker push "$i"; done
```

Use `$KSYNC_IMAGES` (plural) so a build group's images import together; `$KSYNC_IMAGE` (the
first ref) still works for the single-image case. `$KSYNC_IMAGES` is newline-separated, so an
unquoted use word-splits into one argument per image.

ksync stays out of the way here on purpose: it has no built-in idea of "k3d" or "kind", so the
one line above is exactly what runs — and any other tool or transport works the same way without
waiting for a ksync release. The load runs only when an image is actually (re)built, so the
fast manifest-only loop never pays for it. With k3d/kind, set the pods' `imagePullPolicy` to
`Never` or `IfNotPresent` so the kubelet uses the imported image instead of trying to pull it.

One cost to know: `k3d image import` (and `kind load`) transfer a *whole image tarball* per call —
a few seconds each (measured ~3.3s for a ~120 MB image; `k3d image import --mode direct` shaves it
to ~2.8s). It re-sends every layer even when only the top one changed, because a tarball has no
notion of "already present". Editing one service rebuilds and imports just that one image — fast.
A cold `watch` start that builds many images imports them one after another, so first convergence
on a big project takes a little longer; steady-state editing does not.

#### k3d fast-path: push to a registry instead of importing

`k3d image import` is the zero-setup default, but the tarball transfer dominates the loop once the
build itself is incremental. If you want a sub-second load, give the cluster a local registry and
**push** instead — a registry only transfers the layers it does not already have, so an incremental
rebuild moves one layer:

```yaml
# imageLoad: retag each built ref to the local registry and push it.
imageLoad: |
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
   "save all" in your editor becomes one sync, not ten.
4. Only the affected app is re-rendered and applied. If two apps share a chart directory and
   you edit the chart, both apps sync.
5. If a sync fails (for example, the YAML is broken half-way through your edit), ksync prints
   the error, keeps running, and retries with growing wait times. The next file save resets
   the retry and syncs immediately again.

Stop it with Ctrl-C. When ksync is running in an interactive terminal, **press Enter to resync
every app** — handy after restarting a dependency by hand, or to re-pull an image that failed.

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-debounce` | `200ms` | Quiet period after the last change before re-rendering. |
| `-max-parallel` | `4` | How many apps may sync at the same time. |
| `-prune` | `true` | Delete tracked resources that you removed from the files. |
| `-timeout` | `5m` | Max time to wait for one app to become healthy before giving up and retrying. `0` disables the limit. |
| `-v` | `false` | Verbose: also log every detected file change. |

### `ksync sync` — one-time sync

```bash
ksync sync                  # sync all apps once, in needs-order
ksync sync api-b
```

Renders and applies once, then exits. Useful for scripts, or to converge the cluster before
starting `watch`. Apps are synced in dependency order (`needs` first). Apps with `build`
entries build their images first, so what gets applied always points at images that exist.

Each app prints a one-line summary; only failures are listed in detail:

```text
✓ api-b  3 applied, 1 pruned
```

It takes the same `-timeout` (default `5m`) and `-v` flags as `watch`. The timeout matters
most here: a one-time sync waits for the app to become healthy, so without it a pod stuck in
`ErrImagePull` would hang `ksync sync` forever. On timeout the sync fails and names the
resources that never became healthy, so you know where to look.

### `ksync render` — print the YAML

```bash
ksync render api-b          # render one app to stdout
ksync render > all.yaml     # render every app
```

Renders the kustomization (including helm chart inflation) and prints the result. It does not
talk to the cluster — and it does not run docker: the output shows the manifests as written,
without locally built dev tags. Use it to check what ksync *would* apply, or to debug a
kustomization.

The output is the same bytes that `kustomize build --enable-helm --load-restrictor
LoadRestrictionsNone <dir>` produces — this is verified by tests.

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

- ksync talks only to the context named in `ksync.yaml`. There is no flag to override it, and
  the current kubectl context is never used. A config file checked into a repo can therefore
  never point a teammate's ksync at the wrong cluster.
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
