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

apps:
  # The smallest possible app: only a path.
  # The app name defaults to the directory name ("shop" here).
  - path: apps/shop

  # An app with every field set.
  - name: api-b                  # explicit name (used in commands, logs, labels)
    path: apps/api-b             # directory that contains kustomization.yaml
    namespace: team-a            # default namespace for this app (see below)
    needs: [db]                  # sync "db" first

  - name: db
    path: apps/postgres
    namespace: team-a
```

The fields, one by one:

| Field | Required | Meaning |
|---|---|---|
| `context` | yes | The only kubectl context ksync will use. |
| `apps[].path` | yes | Directory with a kustomization file. Relative paths are resolved from the config file's directory. |
| `apps[].name` | no | Name of the app. Default: the directory name. Used in commands (`ksync sync api-b`), in logs, and as the tracking label value. |
| `apps[].namespace` | no | Default namespace for resources that do not set one (like ArgoCD's `destination.namespace`). ksync creates this namespace if it does not exist. |
| `apps[].needs` | no | Apps that must sync before this one. ksync checks that there is no cycle. |

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

## Commands

Every command accepts `-f <file>` to choose the config file, and an optional list of app names.
No app names means **all** apps.

### `ksync watch` — the main loop

```bash
ksync watch                 # watch all apps
ksync watch api-b shop      # watch only these apps
```

What it does:

1. On start, it syncs every watched app once, so the cluster matches your files.
2. Then it watches the app directories for file changes. It also watches files *outside* the
   app directory that the kustomization points to: a shared `chartHome`, values files,
   `resources:` entries, and so on.
3. When you save a file, ksync waits a short quiet period (debounce, default 200ms), so one
   "save all" in your editor becomes one sync, not ten.
4. Only the affected app is re-rendered and applied. If two apps share a chart directory and
   you edit the chart, both apps sync.
5. If a sync fails (for example, the YAML is broken half-way through your edit), ksync prints
   the error, keeps running, and retries with growing wait times. The next file save resets
   the retry and syncs immediately again.

Stop it with Ctrl-C.

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-debounce` | `200ms` | Quiet period after the last change before re-rendering. |
| `-max-parallel` | `4` | How many apps may sync at the same time. |
| `-prune` | `true` | Delete tracked resources that you removed from the files. |

### `ksync sync` — one-time sync

```bash
ksync sync                  # sync all apps once, in needs-order
ksync sync api-b
```

Renders and applies once, then exits. Useful for scripts, or to converge the cluster before
starting `watch`. Apps are synced in dependency order (`needs` first).

Example output:

```text
api-b  /Namespace//team-a            Synced  namespace/team-a serverside-applied
api-b  /ConfigMap/team-a/api-config  Synced  configmap/api-config serverside-applied
api-b  apps/Deployment/team-a/api-b  Synced  deployment.apps/api-b serverside-applied
```

### `ksync render` — print the YAML

```bash
ksync render api-b          # render one app to stdout
ksync render > all.yaml     # render every app
```

Renders the kustomization (including helm chart inflation) and prints the result. It does not
talk to the cluster. Use it to check what ksync *would* apply, or to debug a kustomization.

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
namespaces that ksync created.

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
