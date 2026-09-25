# ksync

ksync is a sync loop for developing Kubernetes manifests against a local cluster. It watches your
kustomize directories. When files change, it renders, diffs, and applies the affected app with
ArgoCD's sync rules: hooks, sync waves, prune, and a wait until resources are Healthy. ksync uses
server-side apply by default. If an app builds from source, ksync rebuilds its image first.

Think of it as ArgoCD pointed at local files instead of git, or `helmfile apply` in a loop. It is
for development only: it has no server or UI, and it does not deploy to production. It also does
not hot-reload code in running pods. mirrord and Telepresence cover that loop by running your
local process against the cluster.

> **Limitation:** kustomize exec plugins (such as ksops) are not supported yet, so a kustomization
> that uses one fails to render.

## Install

- **Nix** (flakes): run `nix run github:motoki317/ksync -- <command>`, or add ksync to a
  project's devShell:

  ```nix
  # flake.nix
  inputs.ksync.url = "github:motoki317/ksync";
  # then add to your devShell's packages:
  #   inputs.ksync.packages.${system}.default
  ```

  Prebuilt binaries are in the `motoki317-ksync` Cachix cache. `nix run` asks you to accept it,
  and Nix uses it only if you are a trusted user. Nix never applies the cache setting of an
  input flake, so for a devShell add the cache to your own Nix config (for example,
  `/etc/nix/nix.conf`):

  ```ini
  extra-substituters = https://motoki317-ksync.cachix.org
  extra-trusted-public-keys = motoki317-ksync.cachix.org-1:uDM0RWapTkolNEgkcqQIGpmJc3bumFf+y3RYj50jQA0=
  ```

  Without the cache, Nix builds ksync from source. Do not set
  `inputs.ksync.inputs.nixpkgs.follows`: it changes ksync's build inputs, so the cached binary no
  longer matches.

- **Binary**: download the archive for your platform (Linux or macOS, amd64 or arm64) from the
  [releases page](https://github.com/motoki317/ksync/releases). Put `ksync` on your `PATH`.

kustomize is built in. Two tools are optional: `helm` 3.17 or later on your `PATH` for
kustomizations that use `helmCharts`, and `docker` for apps that build images from source.

## Quickstart

Write a `ksync.yaml` in the directory where you run ksync:

```yaml
allowedContexts: [docker-desktop]   # the only kubectl context ksync can use
apps:
  - path: apps/shop                 # a directory with a kustomization.yaml
```

Then preview, apply once, and keep watching:

```bash
ksync diff    # preview what sync will change
ksync sync    # apply, wait until Healthy, then exit
ksync watch   # sync, then sync again when files change
```

On a terminal, `watch` asks before it acts on a change. Run `ksync watch --auto` to skip the
question.

## Documentation

The CLI help is the full guide. Run `ksync help` for the commands and topics, and
`ksync <command> -h` for one command's flags. Start with `ksync help config` to write your
`ksync.yaml`, and `ksync help builds` to build images from source.

## Development

Run `nix develop` (or direnv with `.envrc.example`) to get the toolchain and the commit hooks.
[AGENTS.md](AGENTS.md) is the contributor guide: the repo map, the build and test commands, and
the conventions. Design decisions are in [docs/ADR/](docs/ADR/).

## License

[MIT](LICENSE)
