# ksync — local-development sync loop for Kubernetes

ksync is a long-running CLI that watches a set of local directories containing
`kustomization.yaml`, and on file change rebuilds (renders), diffs, and applies the affected app
to a local cluster — with **ArgoCD-parity sync semantics** (helm hooks, sync waves, prune,
server-side apply, health assessment) and a **fast change→applied loop** as the primary design
goal.

Think: "ArgoCD pointed at local files instead of git, as a single fast CLI" or "`kubectl apply`
with inventory tracking, hooks, ordering, retry, and a file watcher."

What ksync is **not**:

- Not a production release/CI tool (CI builds and pushes images; a GitOps controller deploys
  production).
- Not a hot-reload/file-sync-into-container tool (mirrord/Telepresence cover that inner loop).
- Not a GitOps controller (no in-cluster component; push-based, local-first).

> **Status: core loop working.** `watch`, `sync`, `render`, and `destroy` are implemented
> (long-running watch loop with debounced incremental re-render, server-side apply, tracked
> prune, backoff retries). Not yet done: `diff`, namespace auto-creation, exec-plugin
> (e.g. ksops) rendering, hook-semantics conformance fixtures, and the perf validation
> against the target numbers.

## Design pillars

- **Long-running process with a warm cluster cache** — built on
  [gitops-engine](https://github.com/argoproj/argo-cd/tree/master/gitops-engine), keeping cluster
  watches open between syncs so repeated syncs diff against cached live state instead of
  re-listing the cluster.
- **ArgoCD hook semantics, not Helm's** — e.g. `helm.sh/hook: post-install` re-runs on *every*
  sync as PostSync, `helm.sh/hook-weight` acts as the sync wave.
- **Incremental rendering** — only the app(s) whose watched files changed are re-rendered
  (kustomize, including `helmCharts` inflation via `--enable-helm`).
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
