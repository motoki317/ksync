# ksync — local-development sync loop for Kubernetes

ksync is a long-running CLI for developing Kubernetes manifests against a local cluster. It watches
your `kustomization.yaml` directories and, on every save, renders, diffs, and applies the changed
app with ArgoCD's sync semantics (helm hooks, sync waves, prune, server-side apply, health gating) —
optionally rebuilding the image from source first.

Think of it as ArgoCD pointed at local files instead of git, or `helmfile apply` as a continuous
watch loop. It is a development tool only: no server or UI, no production deploys (use a GitOps
controller), and no file-sync into running containers (use mirrord or Telepresence).

> **Status: core loop and native builds working.** `watch`, `sync`, `render`, `images`, and
> `destroy` are implemented. Not yet done: `diff`, exec-plugin (e.g. ksops) rendering, and
> hook-semantics conformance fixtures.

## Quickstart

Declare your apps in `ksync.yaml`:

```yaml
allowedContexts: [docker-desktop]   # the only context ksync may touch; a sole entry is auto-targeted
apps:
  - path: apps/shop
  - name: api-b
    path: apps/api-b
    namespace: team-a          # default ns for resources without one
    needs: [db]                # sync db before api-b
    build:                     # rebuild + redeploy when the source changes
      - image: example.com/team-a/api-b
        context: ../src/api-b
  - name: db
    path: apps/postgres
    namespace: team-a
```

Then run the loop:

```bash
ksync watch   # keep watching; re-render + apply affected apps on save
ksync sync    # one-shot: converge the cluster to the local files, then exit
```

The full guide — every field, every flag, tracking/prune/hook behavior, troubleshooting — is in
**[docs/usage.md](docs/usage.md)**.

## Development

The dev environment is a Nix flake (`nix develop`, or `direnv` via `.envrc.example`); commands live
in the [justfile](justfile):

```bash
just build       # CGO_ENABLED=0 go build → ./ksync
just test        # go test ./... (includes the leak guard, see AGENTS.md)
just check       # gofmt gate + go vet + advisory golangci-lint
```

Commit-time git hooks are installed by the flake devShell: every commit must build and pass tests,
and the Nix build is verified when a commit touches dependency/flake files.

Agent and contributor conventions live in [AGENTS.md](AGENTS.md); design decisions in
[docs/ADR/](docs/ADR/).

## License

[MIT](LICENSE)
