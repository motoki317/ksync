# ksync — local-development sync loop for Kubernetes

ksync is a long-running CLI for developing Kubernetes manifests against a local cluster. It watches
your `kustomization.yaml` directories and, on every save, renders, diffs, and applies the changed
app with ArgoCD's sync semantics (helm hooks, sync waves, prune, server-side apply, health gating) —
optionally rebuilding the image from source first.

Think of it as ArgoCD pointed at local files instead of git, or `helmfile apply` as a continuous
watch loop. It is a development tool only: no server or UI, no production deploys (use a GitOps
controller), and no file-sync into running containers (use mirrord or Telepresence).

> **Status: core loop and native builds working.** `watch`, `sync`, `diff`, `render`, `images`,
> and `destroy` are implemented. Not yet done: exec-plugin (e.g. ksops) rendering and
> hook-semantics conformance fixtures.

## Install

- **Nix** (flakes enabled): run it directly with `nix run github:motoki317/ksync -- <command>`, or
  add it to a project's devShell:

  ```nix
  # flake.nix
  inputs.ksync.url = "github:motoki317/ksync";
  # then, in your devShell's packages:
  #   inputs.ksync.packages.${system}.default
  ```

  Don't set `inputs.ksync.inputs.nixpkgs.follows` — pinning ksync to your nixpkgs rebuilds it from
  source instead of substituting the prebuilt binary from its cache.

- **Binary**: download the archive for your platform from the
  [latest release](https://github.com/motoki317/ksync/releases) and put `ksync` on your `PATH`.

`helm` is needed on `PATH` only when a kustomization inflates `helmCharts`; the Nix devShell bundles
it. `docker` is needed only for apps that build from source. kustomize is built in.

## Quickstart

A minimal `ksync.yaml` lists a context and one app directory:

```yaml
allowedContexts: [docker-desktop]   # the only context ksync may touch; a sole entry is auto-targeted
apps:
  - path: apps/shop                 # a directory with a kustomization.yaml
```

Then run the loop:

```bash
ksync watch   # keep watching; re-render + apply affected apps on save
ksync sync    # one-shot: converge the cluster to the local files, then exit
```

Apps grow fields as you need them — a default namespace, dependency ordering, image builds:

```yaml
apps:
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

The full guide lives in the CLI itself: run **`ksync help`** for the command list and the
concept guides (`ksync help config`, `builds`, `strategy`, `hooks`, `troubleshooting`), and
`ksync <command> -h` for a command's flags and examples.

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
