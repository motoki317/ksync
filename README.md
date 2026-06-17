# ksync — local-development sync loop for Kubernetes

ksync is a long-running CLI that watches a set of local directories containing
`kustomization.yaml`, and on file change rebuilds (renders), diffs, and applies the affected app
to a local cluster — with **ArgoCD-parity sync semantics** (helm hooks, sync waves, prune,
server-side apply, health assessment) and a **fast change→applied loop** as the primary design
goal. With per-app `build` entries it also closes the source loop: save a source file, and
ksync docker-builds the image, injects the new tag, and rolls the pods.

Think: "ArgoCD pointed at local files instead of git, as a single fast CLI" or "docker-compose,
but the runtime is your local Kubernetes cluster."

What ksync is **not**:

- Not a production release/CI tool (CI builds and pushes images; a GitOps controller deploys
  production).
- Not a hot-reload/file-sync-into-container tool (mirrord/Telepresence cover that inner loop).
- Not a GitOps controller (no in-cluster component; push-based, local-first).

> **Status: core loop + native builds working.** `watch`, `sync`, `render`, and `destroy` are
> implemented (long-running watch loop with debounced incremental re-render, server-side
> apply, tracked prune, namespace auto-creation, backoff retries, source-triggered image
> builds with content-addressed dev tags, and an `imageLoad` hook for clusters with a separate
> image store (k3d, kind, remote). Not yet done: `diff`, exec-plugin (e.g. ksops) rendering,
> and hook-semantics conformance fixtures.

## Quickstart

Declare your apps in a `ksync.yaml`:

```yaml
context: docker-desktop        # the ONLY kubectl context ksync will touch
# imageLoad:                   # only for k3d/kind/remote (separate image store)
#   command: k3d image import --cluster dev $KSYNC_IMAGES
apps:
  - path: apps/shop
  - name: api-b
    path: apps/api-b
    namespace: team-a          # default ns for rendered resources without one
    needs: [db]                # sync db before api-b
    build:                     # rebuild + redeploy when the source changes
      - image: example.com/team-a/api-b
        context: ../src/api-b
  - name: db
    path: apps/postgres
    namespace: team-a
```

Then run the loop and edit your manifests:

```bash
ksync sync            # one-shot: converge the cluster to the local files
ksync watch           # keep watching; re-render + apply affected apps on save
ksync render api-b    # print the rendered YAML of one app (no cluster access)
ksync destroy -yes    # delete everything ksync tracks (and nothing else)
```

The full guide — every field, every flag, how tracking/prune/hooks behave, troubleshooting —
is in **[docs/usage.md](docs/usage.md)**.

## Design pillars

- **Long-running process with a warm cluster cache** — built on
  [gitops-engine](https://github.com/argoproj/argo-cd/tree/master/gitops-engine), keeping cluster
  watches open between syncs so repeated syncs diff against cached live state instead of
  re-listing the cluster.
- **ArgoCD hook semantics, not Helm's** — e.g. `helm.sh/hook: post-install` re-runs on *every*
  sync as PostSync, `helm.sh/hook-weight` acts as the sync wave.
- **Incremental rendering** — only the app(s) whose watched files changed are re-rendered
  (kustomize, including `helmCharts` inflation via `--enable-helm`).
- **Native builds without ceremony** — two fields (`image`, `context`) per built image;
  content-addressed dev tags mean unchanged source never rolls a pod, and no build state is
  persisted anywhere. `.dockerignore` decides what triggers rebuilds.
- **Readable output** — clean, colored, single-line status on stderr (the Kubernetes client and
  sync engine are silenced to real errors); a bounded sync timeout turns a stuck pod into a
  clear "still not healthy" message instead of a hang.
- **Safety** — explicit kubectl-context allowlist; prune scoped by a ksync tracking label.

## Development

The dev environment is a Nix flake (`nix develop`, or `direnv` via `.envrc.example`); commands
live in the [justfile](justfile):

```bash
just build       # CGO_ENABLED=0 go build → ./ksync
just test        # go test ./... (includes the leak guard, see AGENTS.md)
just check       # gofmt gate + go vet + advisory golangci-lint
```

Commit-time git hooks are installed automatically by the flake devShell: every commit must
build and pass tests, and the Nix build is verified when a commit touches dependency/flake
files.

Agent/contributor conventions live in [AGENTS.md](AGENTS.md); design decisions in
[docs/ADR/](docs/ADR/).

## License

[MIT](LICENSE)
