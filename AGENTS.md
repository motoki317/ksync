# Agent guide

Users read [README.md](README.md) and the CLI help (`ksync help`).

## What ksync is

ksync is a local-development sync loop for Kubernetes. `ksync watch` is a long-running process
that watches local `kustomization.yaml` directories. On a change, it renders, diffs, and applies
the affected app to a local cluster, with ArgoCD sync semantics. `ksync sync` does one pass and
exits. An app's `build` entries extend the loop to source code: docker build, dev tag, image
injection, sync.

The target for a single-app edit is p50 ≤ 2s and p95 ≤ 5s from change to applied, not counting
hook Job runtime. No automated test checks it. `watch` prints how long each stage took.

If design or docs choices conflict, choose what is easy for users and straightforward, with no
bloated docs. Bloat is redundancy, not depth that a user needs. Fix the redundancy instead of
deleting reference content. `ksync <cmd> -h` must stay self-sufficient. If a docs change forces a
choice between lean and comprehensive, ask the user.

## Non-negotiable constraints

Each one was checked against the source code. Reopen one only with new evidence.

- **Long-running process with a warm cluster cache.** gitops-engine's `pkg/cache` keeps cluster
  watches open between syncs. This is the speed advantage over one-shot CLIs. Never spawn a
  process per change.
- **Hooks follow ArgoCD, not Helm.** For example, Helm `post-install` and `post-upgrade` hooks
  become PostSync hooks and run again on every sync. The full mapping is in
  [docs/argocd-parity.md](docs/argocd-parity.md).
- **Engine:** `github.com/argoproj/argo-cd/gitops-engine`, the maintained copy inside the argo-cd
  repo. Its pin in `go.mod` is the pseudo-version of an argo-cd release-tag commit. On a bump,
  record the release tag in the commit message. The `k8s.io/*` modules follow the engine's pin.
  Keep engine calls behind `internal/engine`.
- **Rendering:** kustomize with `helmCharts` inflation (`--enable-helm`, which needs the helm
  binary). One kustomization can hold many chart releases that share a local `chartHome`. Keep
  kustomize and helm versions close to the ones that ArgoCD bundles. `go.mod` pins the in-process
  kustomize (`sigs.k8s.io/kustomize/api`). The flake's nixpkgs pins the helm and kustomize
  binaries.
- **Apply:** server-side apply. ksync stamps a tracking label on every resource it applies and
  prunes each app by that label. Prune must never touch a resource that ksync does not own.
- **Context safety:** ksync runs only against a kubectl context listed in `allowedContexts`. It
  never reads the kubeconfig's current-context.

## Repo map

- `cmd/ksync/` — the commands. `cli.go` holds the cobra command tree and flags. `help.go` holds
  the help text, which is the full user guide. `help_test.go` checks that each `ksync help X`
  reference names a real topic, that exactly five topics exist, and that each `--flag` in an
  Example exists. `main.go` holds `sync`, `watch`, `render`, and the error-to-exit-code map.
  `diff.go`, `images.go`, `destroy.go`, and `override.go` (`--image`) hold the other commands. The
  remaining files are helpers of these commands. For example, `gate.go` holds the watch prompt and
  reporter, and `diagnostics.go` wires in the diagnostics. `sync`, `watch`, `destroy`, and
  `images --live` use `signalContext`: the first Ctrl-C cancels, and the second exits at once.
  ADRs: 20260701-cli-cobra-and-help-as-docs, 20260627-double-signal-force-quit,
  20260625-diff-command, 20260617-images-command, 20260616-image-override,
  20260702-watch-image-override-takeover, 20260925-app-profiles.
- `internal/config` — the `ksync.yaml` model and its validation. `Select` picks apps by name or
  profile. `SelectContext` enforces `allowedContexts`. ADRs: 20260612-app-model-and-config,
  20260615-allowed-contexts, 20260619-context-auto-select, 20260925-app-profiles.
- `internal/build` — source to image: docker build or a custom `command`, then the dev tag
  `ksync-<12 hex>` from the image fingerprint. It also runs build groups and the `imageLoad`
  command. Loads always run one at a time. A group's command runs one at a time unless the group
  sets `parallel: true`. ADRs: 20260612-build-integration, 20260614-content-address-by-fingerprint,
  20260613-image-load-hook, 20260617-imageload-batching, 20260614-bulk-build-groups,
  20260630-serialize-build-groups.
- `internal/render` — in-process kustomize (krusty) that matches `kustomize build --enable-helm
  --load-restrictor LoadRestrictionsNone`. A test enforces byte parity with the binary. It also
  applies post-render `patches` and injects dev tags (`SetImages`). `cmd/ksync/helmlookup.go`
  writes the helm wrappers for live-cluster rendering. ADRs: 20260612-in-process-kustomize-renderer,
  20260614-live-cluster-helm-render, 20260625-render-error-cleanup, 20260623-post-render-patches,
  20260629-default-var-expansion, 20260630-client-render-per-app.
- `internal/engine` — the gitops-engine wrapper: warm cache, apply, prune, namespace creation,
  hooks, server-side diff, and the health gate. `Sync` retries failures until the app is healthy
  or `--timeout` expires. `Diagnose` reports why an app is still unhealthy after a timeout or a
  Ctrl-C of `sync`. Its live fixtures are in `testdata/diagnostics/`. ADRs:
  20260612-gitops-engine-and-prune-safety, 20260612-sync-performance,
  20260614-sync-loop-hook-convergence, 20260614-sync-health-gate,
  20260614-ensure-referenced-namespaces, 20260616-hook-rerun-on-failure,
  20260624-cr-health-override, 20260625-server-side-diff-default,
  20260623-sync-timeout-diagnostics, 20260627-actionable-diagnostics,
  20260709-sync-convergence-retry.
- `internal/loop` — the `watch` event loop. Builds and deploys use separate schedulers. An image
  builds as soon as its source changes, and its deploy waits for `needs` and for that build. The
  first convergence runs at once. In an interactive terminal without `--auto`, the loop asks
  before it acts on each later change. The cluster and docker are injected (`SyncFunc`,
  `BuildFunc`), so the tests need neither. ADRs: 20260616-eager-build-ahead,
  20260616-manual-build-gate, 20260619-refresh-open-build-prompt.
- `internal/schedule` — the pure scheduler: debounce, one run per app at a time, a parallelism
  limit, `needs` gating, and retry backoff. An external block lets the loop hold a deploy while
  its image builds.
- `internal/watch` — maps a changed path to the affected apps and build entries, finds each app's
  input directories, and runs the recursive fsnotify watcher.
- `internal/ui` — terminal output: the log sink, colors, the live Build → Import → Deploy block,
  the line stream for non-terminal output, and the rebuild picker. Write committed output with
  `ui.WriteLine` and its `ui.Section` kind. The console adds the blank line between sections, so
  never print blank lines by hand. ADRs: 20260615-grouped-pipeline-progress,
  20260616-committed-stage-timings, 20260618-group-total-and-deploy-line,
  20260619-section-output-spacing, 20260619-single-view-build-picker,
  20260701-non-terminal-log-streaming.
- `internal/leakcheck` — the leak guard (see Conventions).

## Build and test

Work inside the flake devShell: `nix develop`, or direnv after you copy `.envrc.example` to
`.envrc`. Outside it, the render parity tests skip, because they need the helm and kustomize
binaries.

```bash
just build       # CGO_ENABLED=0 go build → ./ksync
just test        # go test ./..., includes the leak guard
just test-race   # go test -race ./...
just check       # the commit hook's gofmt and vet, plus golangci-lint (advisory, never fails)
just leakcheck   # the leak guard alone
```

The race detector runs automatically only in CI (`ci.yaml`). The commit hook and `just test` run
plain `go test`, so a failure that only `-race` finds stays green until the push fails CI. Before
you push a change to goroutines, channels, `sync.*`, or scheduling, run `just test-race`. For a
race that shows only sometimes, loop the test: `go test -race -run TestX -count=500 ./...`.

The devShell installs git hooks. Every commit runs `just pre-commit` (build, gofmt, vet,
`go test`), so every commit must build and pass. A second hook runs `just nix-build`, but only
when a commit changes `go.mod`, `go.sum`, `flake.nix`, or `flake.lock`. Only those files can break
the Nix build, for example with a stale `vendorHash`. If `nix build` reports a hash mismatch, set
`vendorHash` in `flake.nix` to the `got:` value.

## Releases

Pushing a `vX.Y.Z` tag publishes a release. The steps and traps are in the `release` skill
([.claude/skills/release/SKILL.md](.claude/skills/release/SKILL.md)).

## Conventions

- Commits use Conventional Commits, in English, one commit per coherent change. Run git write
  commands only when the user or the task's workflow asks for them.
- Commit subjects become the release notes. GoReleaser lists `feat`, `fix`, and `perf` under their
  own headings, drops `docs`, `test`, `chore`, and `ci`, and puts the rest under Other. Give a
  user-facing change a `feat`, `fix`, or `perf` subject.
- **No machine-local or environment leakage. This rule is absolute.** Tracked files (code,
  comments, tests, docs) and commit messages must read the same on every machine. They must not
  reveal anything about the author's clusters or employer.
  - Never write a name that you can see in your kube environment or local checkouts: a namespace,
    cluster, context, node, service, workload, middleware, or product. Never write a cloud ARN,
    account ID, internal hostname, GitHub org, or machine-local path (`/Users/…`, `/home/…`).
  - The rule also covers the tempting cases: a "verified live" note, a test fixture and its
    expected value, a doc example, a benchmark result. To describe a real finding, invent a
    placeholder and use it on both the input side and the expected side.
  - Use these placeholders: `<repo>` for paths, the AWS documentation values (account
    `111122223333`, region `us-west-2`, cluster `prod-cluster`), and invented apps and namespaces
    (`team-a`, `api-b`, `shop`).
  - If you need a real value to reproduce something, keep it in gitignored scratch (`HANDOFF.md`,
    `docs/plans/`).
  - `internal/leakcheck` enforces the rule. It reads the real context, cluster, and namespace
    names and account IDs from your kubeconfig. It fails if one of them is in a committable file,
    untracked files included. It ignores kubeconfig names shorter than 5 characters and generic
    names such as `docker-desktop`, `kind`, and `k3s`. Put names that a kubeconfig cannot show
    (internal product or service names) in the gitignored `.leakcheck` file, copied from
    `.leakcheck.example`. Pass one-off names in `KSYNC_LEAKCHECK_EXTRA`.
  - With no kubeconfig names and no extra names, the leak guard skips. CI has neither, so only a
    local run catches a leak. Run `just leakcheck` before you commit notes or fixtures.
- ADRs are named `YYYYMMDD-title.md` and start from `docs/ADR/_template.md`. To supersede an ADR
  fully, rename it with a leading `_`, set `status: superseded`, and start it with a
  "Superseded by X" note. A partly superseded ADR keeps its name and gets a note inside. Cite a
  renamed ADR by its `_` name. Before design work, read
  [why ksync exists (build vs. buy)](docs/ADR/20260612-build-vs-buy-tool-landscape.md) and the
  [ArgoCD contract](docs/argocd-parity.md).
- A design decision goes in an ADR. A code comment explains a local WHY at that spot in the code:
  a decision that is not obvious, or a hidden constraint. Never narrate WHAT. If a reader can
  infer a comment from the code, delete the whole comment.
- Use TDD for pure logic (render orchestration, dirty-set mapping, scheduling). Use fixtures where
  you can.
