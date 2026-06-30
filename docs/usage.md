# ksync user guide

Concepts and configuration for ksync. Every command's flags live in `ksync <command> -h`; this guide
covers the `ksync.yaml` schema and the behavior the flag help cannot.

## Requirements

- A local Kubernetes cluster and its kubectl context (Docker Desktop, kind, k3d, minikube).
- `helm` on `PATH` — only when a kustomization inflates `helmCharts`. kustomize is built in.
- `docker` — only when an app builds from source.

helm and a reachable cluster are independent needs. `helm` only inflates `helmCharts`. A cluster is
needed to apply or read state (`sync`, `watch`, `diff`, `destroy`, `images --live`) and for the
default live helm `lookup` during render. `ksync render` of a pure-kustomize app needs neither (see
"Commands"). Install: see the [README](../README.md#install).

## Configuration

ksync reads one `ksync.yaml` (default: the working directory; `-f <path>` overrides).

```yaml
allowedContexts:
  - docker-desktop
  # - k3s-*                       # glob: matches several contexts

apps:
  - path: apps/shop               # name defaults to the directory name ("shop")
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

| Top-level field | Description |
|---|---|
| `allowedContexts` | kubectl contexts ksync may target (≥1, required). The safety gate, below. |
| `apps` | The app list (required). |
| `imageLoad.command` | Loads built images into a separate-store cluster (see "Building from source"). |
| `buildGroups` | Named bulk-build commands shared by `build` entries (see "Building from source"). |

| App field | Description |
|---|---|
| `path` | Directory with a kustomization file, relative to the config (required). |
| `name` | App name (default: the directory name). Used in commands, logs, the tracking label. |
| `namespace` | Default namespace for resources that declare none. |
| `needs` | Apps that must sync and become Healthy before this one. Cycles are rejected. |
| `build` | Images built from local source (see "Building from source"). |
| `patches` | Post-render edits to one rendered object (see "Per-environment patches"). |
| `clientRender` | Render this app client-side so it can bundle its own CRDs and custom resources (see "Render, diff, and apply strategy"). |

Unknown fields are rejected.

### Context selection

`allowedContexts` is the safety gate: ksync targets only a listed context and never reads the
kubeconfig current-context. A sole concrete entry is auto-targeted; with several entries or a glob you
must pass `--context <name>`, and it must match an entry. Entries are shell globs — `k3s-*` matches a
family of per-worktree clusters — and a `--context` matched by glob rather than an exact entry warns,
since a glob is a broader grant. `*` matches everything and defeats the gate.

### Default namespace

`namespace` sets the default for rendered resources that declare none (like ArgoCD's
`destination.namespace`); applying a namespaceless resource without it fails. Resources with their own
`metadata.namespace` keep it. ksync creates the namespace if missing; no namespace is ever labeled,
pruned, or deleted (see "Tracking, prune, and safety").

### Per-environment patches

`patches` edits a rendered field the kustomization cannot express per environment — a `hostPath` whose
value depends on where ksync runs. The edit applies after render, before image injection.

```yaml
patches:
  - target: { kind: Deployment, name: cache }   # exact kind+name (+group/version/namespace if set); must match one
    patch: |
      - op: replace
        path: /spec/template/spec/volumes/0/hostPath/path
        value: ${HOME}/.cache/shop
```

`patch` is an inline RFC 6902 op list; lead with a `test` op on a stable field, since it indexes
arrays. `${VAR}` in a `value` expands when ksync renders (from the environment plus `${KSYNC_WORKDIR}`,
the config dir): `${VAR:-default}` falls back when unset or empty, a bare undefined `${VAR}` fails the
render, and `$$` is a literal `$`. `${HOME}` is the cluster host's home, since ksync runs next to it.

## Building from source

A `build` entry rebuilds an image from local source on change, content-tags it, injects the tag, and
rolls the pods.

```yaml
build:
  - image: example.com/team-a/api-b   # as the manifests reference it, without a tag
    context: ../src/api-b             # docker build context; watched for changes
```

| Build field | Description |
|---|---|
| `image` | Image name as manifests reference it (no tag/digest); ksync replaces the tag of every match. Required. |
| `context` | Build context directory, relative to the config. Watched for changes. Required. |
| `name` | Label in progress and prompt output. Default: the image's last path segment. |
| `dockerfile` | Dockerfile relative to `context`. Default: `Dockerfile`. |
| `watch` | Paths under `context` that trigger a rebuild. Default: the whole context. |
| `watchIgnore` | `.dockerignore`-syntax paths kept in the context but excluded from rebuild triggering. |
| `command` | Replaces `docker build` (must leave the image under `$KSYNC_IMAGE`). Excludes `dockerfile`/`group`. |
| `group` | Builds via a top-level `buildGroups` entry. Excludes `command`/`dockerfile`. |

One image name has at most one `build` definition across all apps.

Key behavior:

- The tag is `ksync-` plus a hash of the image's layers and config, so identical content keeps the
  same tag (no pod restart) even when the build tool restamps the image ID. No build state is
  persisted; startup rebuilds once via docker's layer cache.
- An explicit `imagePullPolicy: Always` on a built image is rewritten to `IfNotPresent` — the local
  tag exists in no registry, so `Always` would force a failing pull. Manifest-only edits skip docker.
- `.dockerignore`-excluded files are not watched. A staged build output the Dockerfile `COPY`s but
  that must not retrigger the build goes in `watchIgnore`.

### Custom and grouped builds

`command` replaces `docker build` for one image; a top-level `buildGroups` entry builds several with
one command (a multi-target `docker buildx bake`, or a host compile producing many binaries). Members
reference the group by name; a one-shot `sync` builds every selected entry, while an incremental
`watch` change rebuilds only the dirty members. Both run via `sh -c` in the context and receive
`$KSYNC_IMAGES` (the refs to produce, newline-separated), `$KSYNC_IMAGE` (the first), and `$KSYNC_CONTEXT`.

```yaml
buildGroups:
  - name: services
    command: |
      # $KSYNC_IMAGES = newline-separated <image>:ksync-build tags to produce (only the changed ones)
      for ref in $KSYNC_IMAGES; do
        t=${ref%:*}; t=${t##*/}; sets="$sets --set ${t}.tags=${ref}"; targets="$targets $t"
      done
      docker buildx bake --load $sets $targets
apps:
  - name: services
    path: manifests/services
    build:
      - { image: ghcr.io/team-a/api-b, context: ../.., watch: [services/api-b, lib], group: services }
      - { image: ghcr.io/team-a/api-c, context: ../.., watch: [services/api-c, lib], group: services }
```

A grouped command must leave each requested image tagged `<image>:ksync-build`; ksync content-tags
each afterward. Double-quote `$KSYNC_IMAGE` (single quotes do not expand under `sh -c`).

When two apps share a group, ksync runs that group's command for one app at a time by default — the
apps build concurrently, and a bulk command need not be safe run against itself (a cold `cargo
zigbuild`, for one, races to create its shared cache and fails one invocation). Set `parallel: true`
on the `buildGroups` entry to keep the overlap when the command is concurrency-safe.

### Pre-built image overrides

`ksync sync --image IMAGE=REF` (or `KSYNC_IMAGE_OVERRIDES`, also on `diff`) deploys an existing image
instead of building it — a CI artifact, registry tag, or pinned digest. `REF` may be a bare tag,
`name:tag`, or `@digest`. The build and its `imageLoad` are skipped, so making the ref cluster-visible
is the supplier's job; an override for an unbuilt image is ignored. `watch` rejects overrides.

### Making built images visible (`imageLoad`)

ksync builds into the local docker daemon. Docker Desktop runs pods from it directly; k3d, kind, and
remote clusters keep a separate image store, so a built tag must be loaded. `imageLoad.command` runs
(with `$KSYNC_IMAGES`, `$KSYNC_IMAGE` for the first ref, and `$KSYNC_CONTEXT`) only when an image is
rebuilt; calls are serialized and coalesced.

```yaml
imageLoad:
  command: k3d image import --cluster dev $KSYNC_IMAGES
  # kind:     kind load docker-image --name dev $KSYNC_IMAGES
  # k3s:      docker save $KSYNC_IMAGES | k3s ctr -n k8s.io images import -
  # registry: for i in $KSYNC_IMAGES; do docker push "$i"; done
```

With k3d/kind, set pods to `imagePullPolicy: Never` or `IfNotPresent`. Branch on `$KSYNC_CONTEXT` to
share one config between a shared-daemon and a separate-store cluster (`exit 0` when nothing to load).

## Commands

Every command except `version` takes `-f <file>`, an app-name list (none means all apps), and
`-context`. Run `ksync <command> -h` for flags.

- **`watch`** — the main loop: build and sync once, then re-apply affected apps on change. On an
  interactive terminal a prompt gates each rebuild (Build-all by default, Space to narrow, an empty
  selection skips); `-auto` or a non-terminal rebuilds automatically. Builds run ahead of `needs`
  order, and a deploy never applies an unbuilt image.
- **`sync`** — build, render, and apply once, then exit. Apps run concurrently within the `needs` DAG;
  an app finishes only when its resources are Healthy (up to `-timeout`). On timeout — or Ctrl-C of a
  wedged sync — it names the unhealthy resources and dumps their events and pod logs.
- **`render`** — render to stdout (kustomize plus helm). Read-only, no docker, so built dev tags are
  absent. Renders against the cluster so helm `lookup` resolves; `--offline-render` uses plain `helm
  template` (no cluster; `lookup` returns empty) and byte-matches `kustomize build --enable-helm
  --load-restrictor LoadRestrictionsNone`.
- **`diff`** — a per-resource unified YAML diff against live state: what a sync would create, update,
  or prune. Read-only; secrets are masked; it uses sync's pipeline, so a shown change is one a sync
  would apply.
- **`images`** — canonical references of deployed images, one per line, for cache scoping. Built dev
  tags are excluded. `--live` also reads images of pods in the apps' namespaces, capturing
  operator-derived ones the manifests never name (an ECK Elasticsearch's `spec.version` image).
- **`destroy`** — delete every tracked resource of the apps, in reverse `needs` order. Prints its
  scope and requires `-yes`. Namespaces are never deleted.
- **`version`** — print the version: `dev` from a source build, the tag from a release tarball, the
  git short-rev from a `nix run` build.

`--offline-render` is accepted by `render`, `sync`, `watch`, `diff`, and `images`.

### Render, diff, and apply strategy

ksync runs three phases. Each defaults to the cluster-aware behavior; each has an opt-out for when
that default is wrong for one app or one run.

| Phase | Default | Opt-out |
|---|---|---|
| **Render** | `helm template` against the cluster (so `helm lookup` resolves and capabilities match) | `clientRender: true` (per app), or `--offline-render` (per run) |
| **Diff** | server-side dry-run apply (a field the apiserver defaults or prunes is not drift) | `--client-diff` (per run) |
| **Apply** | server-side apply, prune by the `ksync.dev/app` label | `--prune=false`, `--force` (see "Tracking, prune, and safety") |

**Render — `clientRender: true`.** The default render is a server-side dry-run, so the apiserver maps
every resource. An app whose chart ships a CRD together with custom resources of that kind then fails
with `no matches for kind` until the CRD exists. `clientRender` renders that app client-side — keeping
the cluster's capabilities but dropping the dry-run — the way ArgoCD renders, so one app can own both
its CRDs and its custom resources. It **disables `helm lookup`** for the app, so use it only where the
chart does not need lookup: a chart that uses `lookup` to keep a generated value stable (a webhook
caBundle, an admin password) will regenerate it each sync. It still needs a reachable cluster.

**Render — `--offline-render`.** Renders with no cluster at all (plain `helm`; `lookup` returns empty
and capabilities fall back to helm's defaults), byte-matching `kustomize build --enable-helm
--load-restrictor LoadRestrictionsNone`. For previewing without a cluster; accepted by `render`, `sync`,
`watch`, `diff`, and `images`, and it overrides `clientRender` (every app renders plain).

**Diff — `--client-diff`.** The server-side dry-run keeps a field the apiserver defaults or prunes (a
StatefulSet `maxUnavailable` behind a disabled feature gate) from showing as drift. `--client-diff` uses
the faster in-process merge instead, at the cost of showing such a field as a perpetual change — a
harmless no-op on `sync`/`watch`, visible drift on `diff`.

### Output

Status goes to stderr; `ksync render` manifests go to stdout. Color is on for an interactive terminal
and off under `NO_COLOR`. On a terminal, each app is a live pipeline (🔨 Build → 📦 Import → 🚢 Deploy)
ending in a one-line summary (`✓` healthy, `⚠` Degraded, `✗` failed, with resource counts); a pipe or
CI keeps the Plan, per-app lines, and Summary without the live block.

## Tracking, prune, and safety

Every applied non-Namespace resource gets the label `ksync.dev/app: <app name>` (server-side apply,
field manager `ksync`). Prune (on `sync`/`watch`) and `destroy` act only on resources carrying it with
the matching app name; everything else is invisible to them.

- ksync targets only an `allowedContexts` entry and ignores the kubeconfig current-context.
- Namespaces are never labeled, pruned, or deleted — even one an app renders.
- An app that renders zero resources while it still owns live ones will not prune them away: ksync
  refuses and points at `ksync destroy <app>` for an intentional teardown.
- `destroy` requires `-yes` and prints its scope first.

## Hooks and sync order

ksync follows ArgoCD's rules, not Helm's: hooks read `argocd.argoproj.io/hook` (falling back to
`helm.sh/hook`); `post-install`/`post-upgrade` become PostSync and run on every sync after the main
resources are Healthy; `helm.sh/hook-weight` is the sync wave when `argocd.argoproj.io/sync-wave` is
absent; `helm.sh/hook-delete-policy` maps as in ArgoCD (default `BeforeHookCreation`). A no-change
sync skips hooks, unless a hook's Job is currently failed (it re-runs) or `--force` re-runs them all.

## Troubleshooting

Most errors name their own fix; the recurring ones:

- **`<app> rendered 0 resources but manages N live resource(s)`** — the kustomization now renders
  nothing (a typo, a dropped `resources:` entry). Fix it, or `ksync destroy <app>` to remove the app.
- **`cluster cannot map X (Y)`** — live render asks the cluster to map every kind; a CRD not yet
  installed (a separate base layer) or an unserved apiVersion fails. Install the CRD; if the app bundles
  its own CRDs with custom resources of that kind, set `clientRender: true` on it; or use
  `--offline-render` (helm `lookup` results will be empty).
- **Pods of a built image show `ErrImagePull`** — the kubelet tried to pull the local `ksync-…` tag.
  The image has no matching `build` entry, the cluster has a separate store with no `imageLoad`, or it
  cannot see the docker daemon.
- **Pods restart on every rebuild with no source change** — the image ID changes each build (usually a
  provenance attestation). ksync's `docker build` disables it; with a `command`, add `--provenance=false`.
- **A change is not picked up by `watch`** — ksync watches the app directory and what the kustomization
  references, minus `.dockerignore` and `watch:` exclusions. For anything outside, run `ksync sync`.
- **First sync after start is slow** — ksync lists the cluster once to warm its cache. Keep `watch`
  running; later syncs reuse it.
