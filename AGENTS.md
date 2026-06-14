# Agent guide

Compact navigation aid for AI agents working on this repo — the "where do I look" sheet. Humans
see [README.md](README.md) and the user guide ([docs/usage.md](docs/usage.md)); design rationale
lives in ADRs ([docs/ADR/](docs/ADR/)). Keep this file scannable — push detail to those.

## What ksync is

A local-development sync loop for Kubernetes: a long-running CLI that watches local
`kustomization.yaml` directories and, on change, renders, diffs, and applies the affected app to
a local cluster — with ArgoCD-parity sync semantics and a fast change→applied loop
(**target: p50 ≤ 2s, p95 ≤ 5s for a single-app edit**, excluding hook Job runtime). Per-app
`build` entries extend the loop to source changes: docker build → content-addressed dev tag →
in-process `images:` injection → sync (M2, ADR 20260612-build-integration).

Non-negotiable design constraints (each was verified at source level before scaffolding —
re-litigate only with new evidence):

- **Long-running process, warm cluster cache.** gitops-engine's `pkg/cache` keeps cluster
  watches open between syncs; that is the core perf advantage over every one-shot CLI. Never
  spawn-per-change.
- **Hook semantics follow ArgoCD, not Helm.** Hooks are detected from annotations on rendered
  objects (`argocd.argoproj.io/hook`, falling back to `helm.sh/hook`, excluding `crd-install`);
  `post-install`/`post-upgrade` → PostSync and **re-run on every sync** (after main resources
  are Healthy); `helm.sh/hook-weight` acts as the sync wave when `argocd.argoproj.io/sync-wave`
  is absent; `helm.sh/hook-delete-policy` maps 1:1, default `BeforeHookCreation`.
- **Engine:** `github.com/argoproj/argo-cd/gitops-engine` (the standalone `argoproj/gitops-engine`
  repo was archived Sep 2025; the argo-cd monorepo path is the maintained one). Pin argo-cd
  release tags; wrap engine usage behind a thin internal package to localize v0.x API churn.
- **Rendering:** kustomize with `helmCharts` inflation (`--enable-helm`; helm binary required) is
  the reference manifest shape — multiple chart releases per kustomization, shared local
  `chartHome`. Pin kustomize/helm versions close to what production ArgoCD bundles.
- **Apply:** default to server-side apply; stamp a ksync tracking label on managed resources and
  prune per app via it. Prune must never touch resources ksync doesn't own.
- **Context safety:** only run against an explicitly allowlisted kubectl context.

## Repo at a glance

- `cmd/ksync/main.go` — CLI entry and subcommand wiring (`watch / sync / render / destroy`
  implemented; `diff` still a stub).
- `internal/config` — ksync.yaml model: app list, single explicit kubectl context (the safety
  model), optional top-level `imageLoad` command (separate-image-store clusters), per-app
  default namespace (ArgoCD destination.namespace parity), `needs` DAG, per-app `build` entries
  (image/context + optional dockerfile/watch/watchIgnore/command); `SortByNeeds`.
- `internal/build` — source→image: docker build (or the `command` escape hatch producing
  `$KSYNC_IMAGE`), content-addressed dev tags `ksync-<12 hex of image ID>` (no persisted
  build state; `--provenance=false` keeps IDs deterministic), `.dockerignore`-scoped watch
  derivation (WatchScope), and `Loader` (the `imageLoad` hook: runs per built ref with
  `$KSYNC_IMAGE` set, for k3d/kind `image import` / registry push). See ADRs
  20260612-build-integration and 20260613-image-load-hook.
- `internal/render` — in-process kustomize (krusty) replicating
  `kustomize build --enable-helm --load-restrictor LoadRestrictionsNone`; byte-parity with the
  binary is enforced by test. `SetImages` (the build-tag injection path) also rewrites an
  explicit `imagePullPolicy: Always` to `IfNotPresent` for images ksync builds — the
  content-addressed local tag exists in no registry, so `Always` would force a doomed pull.
- `internal/ui` — human-facing output: a `logr.LogSink` that renders clean, colored,
  single-line records (a quiet variant drops Info/V noise; used for the engine and the routed
  klog/client-go stream), color helpers (NO_COLOR + TTY aware), and `Activity`, the nix-style
  single-line progress for external build/import commands (full log shown only on failure).
- `internal/watch` — dirty-set mapping (changed path → affected apps/build entries, with
  per-entry ignore predicates), dependency-root derivation (escaping
  chartHome/resources/values), recursive fsnotify watcher with per-root directory pruning.
- `internal/schedule` — pure scheduling state machine: debounce/coalesce, per-app
  serialization, bounded parallelism, needs gating, exponential retry backoff.
- `internal/engine` — gitops-engine wrapper (pin: argo-cd release-tag commits; k8s.io/* follow
  the engine's version): warm cluster cache, SSA, tracking-label-scoped prune, app-namespace
  auto-creation (create-if-missing only).
- `internal/loop` — the watch-mode event loop tying the above together; cluster and docker
  sides injected as SyncFunc/BuildFunc so it tests without either. Builds run per dirty
  (app, entry) before render; manifest-only edits never invoke docker.
- `internal/leakcheck` — the no-leak guard (see Conventions).
- `docs/ADR/` — dated decision records (`YYYYMMDD-title.md`, template at `_template.md`).
- `docs/plans/` — gitignored single-session scratch.
- `HANDOFF.md` — **gitignored, local-only**: the full research context (tool landscape survey
  with citations, verified semantic contract, architecture sketch, milestone plan). It references
  private environment details, which is why it is never tracked. If present on this machine,
  read it before design work; never copy its private references into tracked files.

## Build / test

```bash
just build       # CGO_ENABLED=0 go build → ./ksync  (the authority)
just test        # go test ./... (includes internal/leakcheck)
just check       # gofmt gate + go vet + (advisory) golangci-lint — run before committing Go
just leakcheck   # the no-leak guard alone
```

The flake devShell (`nix develop` / direnv) installs commit-time git hooks: every commit runs
`just pre-commit` (build, gofmt, vet, go test — each commit must compile and pass), and a second
hook runs `just nix-build` ONLY when the commit touches `go.mod`/`go.sum`/`flake.nix`/`flake.lock`
— the only changes that can break the Nix path (e.g. a stale `vendorHash`; sound because the
flake sets `proxyVendor`, pinning `vendorHash` to go.mod/go.sum alone). There is no pre-push
hook; the guarantee lives at commit time so non-building commits never land in history.

## Conventions

- Conventional Commits, **English**. Commit per coherent slice. Git ops ONLY when explicitly
  asked, or when moving between phases.
- **No machine-local or environment leakage — this is absolute.** Git-tracked files (code,
  comments, tests, docs) **and commit messages** must read identically on any machine and reveal
  nothing about the author's clusters or employer. **NEVER write a locally-available resource
  name into the repo** — not a namespace, cluster, context, node, service, workload, middleware,
  or product name you can see in *your* kube environment or local checkouts, and not a cloud
  ARN / account ID / internal hostname / GitHub org / machine-local path (`/Users/…`, `/home/…`).
  This binds **everywhere**, including the tempting cases: a dogfooding/"verified live" note, a
  **test fixture or its expected value**, an example in a doc, a benchmark result. Describing a
  real finding is never a license to name the real resource — invent a placeholder and use it
  consistently on both the input and the expected side. Use generic, clearly-fictional
  placeholders: `<repo>` for paths; AWS docs identifiers (account `111122223333`, region
  `us-west-2`, cluster `prod-cluster`); invented apps/namespaces (`team-a`, `api-b`, `shop`).
  A real value needed to reproduce something stays in gitignored scratch (`HANDOFF.md`,
  `docs/plans/`), never a tracked file.
  - **Enforced by a test:** `internal/leakcheck` derives the real kube context/cluster/namespace
    names and account IDs from your local kubeconfig (it hardcodes none — it stays
    cluster-agnostic) and fails if any reaches a committable file. It scans **untracked files
    too** (gitignored ones excluded), so a leak is caught before it is ever staged. Names a
    kubeconfig can't surface (internal product/service names) go in the gitignored `.leakcheck`
    denylist — copy `.leakcheck.example`; one-offs via `KSYNC_LEAKCHECK_EXTRA`. Run
    `just leakcheck` before committing notes or fixtures.
- ADRs are dated `YYYYMMDD-title.md`; design rationale lives there, not in comments.
- Code comments explain WHY (non-obvious decisions, hidden constraints) — never WHAT.
- TDD for pure logic (render orchestration, dirty-set mapping, scheduling); fixture-driven where
  possible.

## Where durable state lives (docs layout)

Long-lived, shareable context must be **git-tracked** — but anything referencing the private
reference environment stays in gitignored local files:

- **`docs/ADR/`** (tracked) — dated decision records.
- **git log** (tracked) — the authoritative per-change "what + why".
- **`HANDOFF.md`** (gitignored) — research context with private references; local-only.
- **`docs/plans/`** (gitignored) — volatile single-session scratch.
