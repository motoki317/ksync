# ksync user guide

Reference for `ksync.yaml` configuration, commands, and runtime behavior.

## Requirements

- A local Kubernetes cluster and its kubectl context (Docker Desktop, kind, k3d, minikube).
- `helm` on `PATH` — only if kustomizations use `helmCharts`. kustomize is built into ksync.
- `docker` CLI — only if apps use `build`.
- Go 1.26+ or Nix to build ksync.

## Install

```bash
go build -o ksync ./cmd/ksync   # or: just build
nix build .#ksync               # binary at ./result/bin/ksync
```

## Configuration: `ksync.yaml`

ksync reads one config file listing the apps. Default path: `ksync.yaml` in the working directory.
Override with `-f <path>`.

```yaml
allowedContexts:
  - docker-desktop
  - k3d-dev
  # - k3s-*                      # glob: matches several contexts

# imageLoad:                     # only for separate-store clusters (k3d/kind/remote)
#   command: k3d image import --cluster dev $KSYNC_IMAGES

apps:
  - path: apps/shop              # name defaults to the directory name ("shop")

  - name: api-b
    path: apps/api-b
    namespace: team-a
    needs: [db]
    build:
      - image: example.com/team-a/api-b
        context: ../src/api-b

  - name: db
    path: apps/postgres
    namespace: team-a
```

### Top-level fields

| Field | Required | Description |
|---|---|---|
| `allowedContexts` | yes | kubectl contexts ksync may target (≥1). See "Context selection". |
| `imageLoad.command` | no | Command that loads built images into a separate-store cluster. See "Making built images visible". |
| `buildGroups` | no | Named bulk-build commands shared by `build` entries. See "Build groups". |
| `apps` | yes | The app list. |

### App fields

| Field | Required | Description |
|---|---|---|
| `path` | yes | Directory containing a kustomization file. Relative to the config file's directory. |
| `name` | no | App name. Default: the directory name. Used in commands, logs, and the tracking label. |
| `namespace` | no | Default namespace for resources that set none. See "The namespace field". |
| `needs` | no | Apps that must sync and become Healthy before this one. Cycles are rejected. |
| `build` | no | Images built from local source. See "Building images from source". |
| `patches` | no | Post-render edits to one rendered object. See "Per-environment patches". |

Unknown fields are rejected.

### Context selection

`allowedContexts` is the safety gate. ksync targets only a listed context and ignores the kubeconfig
current-context.

- One concrete entry: targeted automatically.
- Several entries, or a single glob: ambiguous; pass `--context <name>`. Without it, the run is
  refused.
- A `--context` value must match an entry.

Entries are shell-style globs (`*`, `?`, `[…]`); a plain name matches exactly. A glob matches a
family of clusters — per-worktree microVMs `k3s-feature-a`, `k3s-feature-b` via `k3s-*`. `*` matches
every context and defeats the gate.

### The namespace field

Some rendered resources have no namespace (e.g. `configMapGenerator` output). `namespace` sets their
default, like ArgoCD's `destination.namespace`. Applying a namespaceless resource without it fails.

ksync creates the namespace during sync if missing. It never modifies or deletes an existing
namespace. Resources with their own `metadata.namespace` keep it.

### Per-environment patches

`patches` edits a rendered field the kustomization cannot express per environment — e.g. a `hostPath`
whose path depends on where ksync runs. The edit applies after render, before image injection and
apply.

```yaml
patches:
  - target: { kind: Deployment, name: cache }
    patch: |
      - op: test
        path: /spec/template/spec/volumes/0/name
        value: cache-vol
      - op: replace
        path: /spec/template/spec/volumes/0/hostPath/path
        value: ${HOME}/.cache/shop
```

- `target` matches by exact `kind` + `name`, plus `group`/`version`/`namespace` when set (an empty
  one matches any). It must match exactly one object; zero or several is an error. `namespace`
  matches the object's own `metadata.namespace`.
- `patch` is an inline RFC 6902 op list. It addresses array elements by index, so lead with a `test`
  op on a stable field; a reorder then fails the patch instead of editing the wrong element.
- `${VAR}` in an op `value` expands at sync time from the process environment, plus `${KSYNC_WORKDIR}`
  (the config file's directory). An undefined variable fails the sync. `$$` is a literal `$`.
  Expansion applies only to `value`, not `path`/`from`.

`${HOME}` resolves to the cluster host's home, since ksync runs next to the cluster. `ksync render`
shows the patched output.

## Building images from source

A `build` entry rebuilds an image from local source on change, injects the new tag, and rolls the
pods.

```yaml
build:
  - image: example.com/team-a/api-b
    context: ../src/api-b
```

### Build fields

| Field | Required | Description |
|---|---|---|
| `image` | yes | Image name as the manifests reference it, without a tag or digest. ksync replaces the tag of every match in the rendered output. |
| `name` | no | Label in progress output and the watch prompt. Default: the image's last path segment. Unique within an app. |
| `context` | yes | docker build context directory. Relative to the config file's directory. Watched for changes. |
| `dockerfile` | no | Dockerfile path, relative to `context`. Default: `Dockerfile`. |
| `watch` | no | Paths (relative to `context`) that trigger a rebuild. Default: the whole context. |
| `watchIgnore` | no | `.dockerignore`-syntax paths excluded from rebuild triggering but kept in the build context. See "Staged build outputs". |
| `command` | no | Replaces `docker build`. See "Custom build commands". Mutually exclusive with `dockerfile` and `group`. |
| `group` | no | Builds this image via a `buildGroups` entry. See "Build groups". Mutually exclusive with `command` and `dockerfile`. |

### Build behavior

1. A watched source file changes; `docker build` runs on the context.
2. The image is tagged by content: `ksync-` plus 12 hex digits hashing its layers and runtime
   config. Identical content yields an identical tag.
3. ksync renders the app and replaces the tag in memory; manifest files are not modified.
4. The app syncs. The changed tag rolls the pods.

Consequences:

- Unchanged content keeps the same tag, so no pod restarts.
- No build state is persisted. On startup ksync rebuilds every image once (fast via docker's layer
  cache) and lands on the deployed tags.
- Tagging uses the image's layers and config, not its image ID. Some builders (`docker buildx bake`)
  restamp the ID each build; fingerprinting keeps unchanged content on a stable tag.
- `imagePullPolicy: Always` is rewritten to `IfNotPresent` on containers running a built image. The
  `ksync-…` tag exists only locally, so `Always` would force a failing registry pull. Only explicit
  `Always` is changed.
- Manifest-only edits skip docker; the last built tag is reused.
- Build and image-load output collapses to one live line; the full log prints only on failure.

### `.dockerignore`

A `.dockerignore`-excluded file never enters the image, so ksync ignores it when watching.
`<dockerfile>.dockerignore` overrides `<context>/.dockerignore` (BuildKit rule).

### Staged build outputs (`watchIgnore`)

A build output staged inside the context (a host-compiled binary the Dockerfile `COPY`s) cannot be
`.dockerignore`d without dropping it from the `COPY`, yet writing it must not retrigger the build.
`watchIgnore` keeps such paths in the context but excludes them from change detection.

```yaml
build:
  - image: example.com/team-a/api-b
    context: .
    watch: [apps/api-b, lib]
    group: native
    watchIgnore: ["**/.zigbuild"]
```

### Custom build commands

`command` replaces `docker build`. ksync runs it with `sh -c` in the context directory. The command
must leave the finished image in the local docker daemon under `$KSYNC_IMAGE`.

```yaml
build:
  - image: example.com/team-a/api-b
    context: ../..
    watch: [apps/api-b, lib]
    command: docker buildx bake --load --set "api-b.tags=$KSYNC_IMAGE" api-b
```

- Double-quote `$KSYNC_IMAGE`. `sh -c` does not expand variables in single quotes. Use single quotes
  only for arguments that must not expand (`--set '*.platform=…'`).
- The build need not be reproducible: ksync tags by layers and config, not image ID.
  `--provenance=false` is optional.

### Build groups

A build group builds several images with one command — a multi-target `docker buildx bake`, or a host
compile producing many binaries. Declare it at the top level; members reference it with `group`.

```yaml
buildGroups:
  - name: services
    # $KSYNC_IMAGES = newline-separated <image>:ksync-build temp tags to produce
    # (only the changed images).
    command: |
      sets=""; targets=""
      for ref in $KSYNC_IMAGES; do
        t=${ref%:*}; t=${t##*/}
        sets="$sets --set ${t}.tags=${ref}"
        targets="$targets $t"
      done
      docker buildx bake --load --set '*.attest=' $sets $targets

apps:
  - name: services
    path: manifests/services
    build:
      - image: ghcr.io/team-a/api-b
        context: ../..
        watch: [services/api-b, lib]
        group: services
      - image: ghcr.io/team-a/api-c
        context: ../..
        watch: [services/api-c, lib]
        group: services
```

- A grouped entry sets neither `command` nor `dockerfile`. `context` and `watch` still decide what
  dirties it.
- Only the dirty subset builds: one changed service yields one ref in `$KSYNC_IMAGES`; changed shared
  code builds all dirty members in one invocation.
- ksync content-tags each image after the command, so the no-rollout-on-unchanged rule holds per
  image.
- All members share one `context`.
- The command must leave each requested image tagged `<image>:ksync-build`.
- `$KSYNC_IMAGES` is newline-separated, so an unquoted `for ref in $KSYNC_IMAGES` word-splits one ref
  per iteration.

### Using a pre-built image

An override deploys an existing image instead of building it — a CI artifact, registry tag, or pinned
digest. It applies to one image; the app's other builds are unaffected. Accepted by `sync` only.

```bash
ksync sync --image ghcr.io/team-a/api-b=ghcr.io/team-a/api-b:ci-1234

export KSYNC_IMAGE_OVERRIDES="ghcr.io/team-a/api-b=ghcr.io/team-a/api-b:ci-1234"
ksync sync
```

- `IMAGE` is the `build` entry's `image:`. `REF` is a bare tag, `:tag`, `name:tag`, `@digest`, or
  `name@digest`. A differing name redirects the repo. A flag overrides the env var for the same image.
- An overridden image is not built, and its `imageLoad` is skipped — making it cluster-visible is the
  supplier's responsibility.
- An override for an image no app builds is ignored.
- `watch` rejects overrides: it has no `--image` flag and fails if `KSYNC_IMAGE_OVERRIDES` is set.

### Making built images visible

ksync builds into the local docker daemon. Docker Desktop runs pods from it directly. k3d, kind, and
remote clusters keep a separate image store, so a built `ksync-<hash>` tag is invisible until loaded.

`imageLoad.command` runs with `$KSYNC_IMAGES` (newline-separated built refs) and `$KSYNC_IMAGE` (the
first).

```yaml
# k3d:
imageLoad:
  command: k3d image import --cluster dev $KSYNC_IMAGES
# kind:
imageLoad:
  command: kind load docker-image --name dev $KSYNC_IMAGES
# k3s:
imageLoad:
  command: docker save $KSYNC_IMAGES | k3s ctr -n k8s.io images import -
# remote registry:
imageLoad:
  command: for i in $KSYNC_IMAGES; do docker push "$i"; done
```

- `$KSYNC_IMAGES` is newline-separated; an unquoted use word-splits per image. A single-image command
  loops over it.
- `$KSYNC_CONTEXT` (the selected context) is exported to `imageLoad` and build commands, so one config
  branches per cluster.
- The load runs only when an image is rebuilt.
- With k3d/kind, set pods to `imagePullPolicy: Never` or `IfNotPresent`.

Loads are serialized (some importers are not concurrency-safe) and coalesced (images finishing during
a load batch into the next). A registry push transfers only changed layers and is faster than `k3d
image import`, which re-sends the whole image tarball.

```yaml
allowedContexts: [docker-desktop, k3d-dev]
imageLoad:
  command: |
    [ "$KSYNC_CONTEXT" = "docker-desktop" ] && exit 0   # shared daemon: nothing to load
    k3d image import --cluster dev $KSYNC_IMAGES         # separate store: import
```

### Limit

One image name has at most one `build` definition across all apps.

## Commands

All commands accept `-f <file>` and an optional app-name list (none means all apps).

### `ksync watch`

Builds and syncs all selected apps once, then watches for changes and re-applies affected apps.
Watched paths: the app directories, the files the kustomization references (chartHome, values,
`resources:`), and `build` source directories.

On change:

1. Debounce (`-debounce`, default 200ms) coalesces a burst into one event.
2. On an interactive terminal, a prompt lists changed images (🔨) and manifest-only apps (🚢). **Build
   all** is the default (Enter); toggle rows with Space to narrow; an empty selection skips. Nothing
   rebuilds until chosen.
3. Chosen images rebuild and their apps re-apply. Unchosen changes stay pending and reappear when
   idle.
4. A failed sync prints the error, retries with backoff, and resyncs immediately on the next change.

Builds run ahead of `needs` order: an image builds as soon as its source changes; only the deploy
waits for dependencies. A deploy never applies an unbuilt image.

Without an interactive terminal (pipe, redirect, process manager) or with `-auto`, every change
rebuilds automatically with no prompt. Stop with Ctrl-C.

| Flag | Default | Description |
|---|---|---|
| `-auto` | `false` | Rebuild on every change without the prompt. Forced on when stdin is not a terminal. |
| `-debounce` | `200ms` | Quiet period before prompting. |
| `-max-parallel` | CPU cores | Concurrent builds, and separately concurrent deploys (independent budgets). Also bounds an app's concurrent image builds. `0` = unlimited. |
| `-prune` | `true` | Delete tracked resources removed from the files. |
| `-timeout` | `5m` | Max wait for an app to become healthy before retrying. `0` = no limit. |
| `-client-diff` | `false` | Decide the apply set with the client-side diff instead of a server-side dry-run apply. Faster, but a field the cluster defaults or prunes is re-applied on every sync. |
| `-v` | `false` | Log every detected file change. |

### `ksync sync`

Renders and applies all selected apps once, then exits.

- Independent apps build, render, and apply concurrently (`-max-parallel`). The `needs` DAG holds a
  dependent until its dependencies finish. A `needs` entry outside the selected set is not added.
- Apps with `build` entries build first.
- An app finishes only when its resources are Healthy — Deployments rolled out, StatefulSets up, Jobs
  complete — up to `-timeout`. An app that never becomes Healthy fails at `-timeout`, names the
  unhealthy resources, and prints a diagnostic dump (each unhealthy resource's recent events; related
  pods' container state and current/previous log tails). A healthy re-sync returns immediately.

Hooks:

- A no-change sync skips hooks.
- A hook whose Job is currently failed (Degraded) re-runs on the next sync.
- `--force` re-runs every hook regardless of diff.

```bash
ksync sync --force            # re-run all hooks for the selected apps
ksync sync --force sistema    # scoped to just that app
```

Referenced namespaces an app does not own are created if missing — untracked, so prune ignores them.

The apply set is decided **server-side** by default — a dry-run server-side apply on the first
reconcile, so a field the apiserver defaults or prunes is not re-applied on every sync (the same
strategy `ksync diff` previews). `--client-diff` decides it with the in-process client-side diff
instead: faster (no dry-run), but such a field is re-applied each sync (a harmless server-side-apply
no-op). The dry-run runs once per app per sync, not on the health-wait polls.

Flags: `-timeout`, `-max-parallel`, `-v` (as `watch`), plus `--force`, `--client-diff`, and
`--image`/`KSYNC_IMAGE_OVERRIDES`. `-max-parallel` defaults to CPU cores, which saturates the
CPU-bound render; raise it only when builds and health waits dominate.

### Output

Status goes to stderr; `ksync render` manifest output goes to stdout. Color is on for an interactive
terminal; `NO_COLOR` (any value) disables it.

On a terminal, each app's work is a live pipeline grouped under its name:

```text
⠹ duo
    ✓ 🔨 Build   rust-core             1.7s
    ⠹ 🔨 Build   rust-services (5)     2.3s
    ⠼ 📦 Import  rust-core             6.0s
    ○ 🚢 Deploy
```

- Stages: 🔨 Build → 📦 Import → 🚢 Deploy. A not-yet-reached stage shows `○`. A build-less app is one
  Deploy line.
- A finished app commits a one-line summary. Status symbols: `✓` healthy, `⚠` applied but Degraded,
  `✗` sync failed. `applied` counts changed resources; `pruned`/`failed`/`degraded` show when nonzero.
- A multi-app run prints a Plan, a pinned Summary updated as apps finish, and a committed Summary on
  completion. A pipe or CI omits the live block but keeps the Plan, per-app lines, and Summary.

### `ksync render`

Renders selected apps to stdout (kustomize plus helm inflation). Read-only; does not run docker, so
built dev tags are absent.

By default helm charts render against the cluster (`--dry-run=server`) so `lookup` resolves.
`--offline-render` uses plain `helm template` instead (charts requiring `lookup` will not resolve);
offline output matches `kustomize build --enable-helm --load-restrictor LoadRestrictionsNone <dir>`.
`--offline-render` is accepted by `render`, `sync`, `watch`, and `images`.

`render` and `images` render selected apps concurrently (`-max-parallel`). Output order is unaffected.

### `ksync images`

Prints the canonical references of deployed container images — sorted, deduplicated, one per line on
stdout. Canonical means the fully-qualified form a runtime stores (`redis:7` →
`docker.io/library/redis:7`), for string-equality matching against a cluster image store.

- Images ksync builds (`build:` entries) are excluded — local dev tags, never pulled.
- Plain `images` lists only manifest-named images. `--live` also reads running-pod images, capturing
  operator-derived images absent from the manifests (e.g. an ECK `Elasticsearch`'s `spec.version`
  image).
- Like `render`, `images` renders against the cluster by default; `--offline-render` renders offline.
  `--live` reads pods and always needs a reachable cluster.

### `ksync destroy`

Deletes every resource ksync tracks for the selected apps, in reverse dependency order. Namespaces are
never deleted. Requires `-yes`. Takes `-timeout` (default `5m`).

```bash
ksync destroy               # refuses; lists the apps it would delete
ksync destroy -yes
ksync destroy -yes shop
```

### `ksync diff`

Renders the selected apps and prints a per-resource unified YAML diff against live cluster state —
what a sync would create, update, or prune. Read-only: no apply, no build, no hooks. Output goes to
stdout.

```bash
ksync diff            # all apps
ksync diff shop api-b
```

- Uses sync's own pipeline (tracking label, default namespace, reconcile, the same diff engine), so a
  reported change is one a sync would apply.
- Server-managed and ksync-bookkeeping fields (`managedFields`, `status`, `resourceVersion`, the
  tracking label, …) are stripped; only the authored change shows.
- Secret values are masked (distinct values get distinct-length masks, so a value-only change still
  reports as an update — `password: ++++++++` ⟶ `password: ++++++++++++` — without exposing the value).
- `--prune` (default true) includes resources a sync would delete. `--image IMAGE=REF` (also via
  `KSYNC_IMAGE_OVERRIDES`) diffs as if that pre-built image were deployed, as on `sync`.
- Renders against the cluster by default; `--offline-render` renders offline. `-max-parallel` bounds
  concurrency.

`build:` images are not rebuilt. When the cluster runs one at a ksync dev tag (`ksync-<hash>`), that
tag is carried forward so its per-build churn does not show; the diff then reflects the currently
deployed image, not an unbuilt source edit. An image at any other tag (no live image yet, or a
foreign tag) is shown at its source ref, and a note warns that ref is not what a sync deploys — sync
rebuilds to a fresh dev tag.

By default the diff is computed **server-side**: a dry-run server-side apply asks the cluster for the
predicted result, so a field the apiserver defaults or prunes (a StatefulSet `maxUnavailable` behind a
disabled feature gate) is not reported as drift. `--client-diff` uses the in-process client-side
three-way merge instead — faster, but it cannot know the apiserver will drop a field, so such a field
shows as a perpetual change. A resource whose dry-run fails (a webhook, RBAC, a field-manager
conflict) falls back to client-side for that resource alone. Server-side is the more faithful preview,
but being faithful it also surfaces changes the client-side merge hides (a field SSA would remove that
the manifest does not declare).

## Tracking and prune

Every applied resource gets the label `ksync.dev/app: <app name>`. Prune and destroy delete only
resources carrying it with the matching app name; everything else is invisible to them. Auto-created
namespaces are unlabeled and never pruned.

Apply uses server-side apply with field manager `ksync`.

## Hooks and sync order

ksync follows ArgoCD's rules, not Helm's:

- Hooks read from `argocd.argoproj.io/hook`, falling back to `helm.sh/hook`.
- `post-install`/`post-upgrade` become PostSync and run on every sync, after the main resources are
  healthy.
- `helm.sh/hook-weight` is the sync wave when `argocd.argoproj.io/sync-wave` is absent.
- `helm.sh/hook-delete-policy` works as in ArgoCD; default `BeforeHookCreation`.

## Safety

- ksync targets only a context in `allowedContexts` and ignores the kubeconfig current-context.
- Prune and destroy touch only resources labeled `ksync.dev/app`.
- `destroy` requires `-yes`.

## Troubleshooting

**`an empty namespace may not be set when a resource name is provided`**
A rendered resource has no `metadata.namespace`. Set `namespace:` on the app.

**`context "X" does not exist`**
The context is not in your kubeconfig. Check `kubectl config get-contexts`.

**`no kustomization file in <dir>`**
The app `path` must contain `kustomization.yaml` (or `.yml`, or `Kustomization`).

**Helm chart errors during render**
ksync runs `helm` for `helmCharts` inflation. Install `helm` and ensure the chart's `values.yaml`
exists.

**A change is not picked up by `watch`**
ksync watches the app directory and everything the kustomization references. It ignores `.git` and
editor temp files. For a file referenced another way (a symlink target outside all watched
directories), run `ksync sync`.

**First install of a chart that ships CRDs fails for its custom resources**
The cluster needs a moment to activate a new CRD applied in the same pass. It self-heals: `watch`
retries, or run `ksync sync` again.

**`cluster cannot map X (Y)`**
Live render (the default) asks the cluster to map every rendered kind. A custom resource whose CRD is
not yet installed (commonly one from a separate base layer) fails on a fresh cluster; the same error
also appears for a built-in apiVersion the cluster does not serve. Install the CRD first (apply the
base layer) if it is a custom resource, or render offline with `--offline-render` (helm `lookup`
results will be empty).

**The same resources re-apply on every sync**
Some charts render fresh content each time (commonly a self-signed webhook certificate). ksync applies
the real difference, as ArgoCD would. Harmless; use a stable certificate (cert-manager) to stop it.

**First sync after start is slow**
ksync lists the cluster's resources once on start to build its cache (a few seconds on a cluster with
many CRDs). Later syncs use the warm cache. Keep `watch` running.

**Pods of a built image show `ErrImagePull` / `ImagePullBackOff`**
The kubelet tried to pull the `ksync-…` tag from a registry. Causes: the image has no matching `build`
entry, so ksync never built it; the cluster has a separate image store and `imageLoad` is unset; or
the cluster cannot see the docker daemon.

**Pods restart on every rebuild with no change**
The image ID changes each build, usually from docker's provenance attestation. ksync's `docker build`
disables it; with a `command`, add `--provenance=false`.

**A source edit does not trigger a rebuild**
ksync watches the build `context` minus `.dockerignore` exclusions, and only `watch:` paths when set.
Check whether the file is excluded or outside the watched paths. `ksync sync` always builds.

**`sync of "X" timed out; still not healthy: …`**
A resource never became healthy (`ErrImagePull`, a crash loop). Fix the named resource, then sync
again. Raise `-timeout` for slow rollouts, or set `0` to wait forever.
