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
  implemented; `diff` still a stub). `override.go` resolves **image overrides** (`--image IMAGE=REF`
  on sync/watch, or `KSYNC_IMAGE_OVERRIDES`): a supplied ref deploys a pre-built image instead of
  building that `build:` entry — the build (and its `imageLoad`) is skipped and the ref is injected
  at deploy; an override for an image no app builds is dropped with a note. This is what lets a
  wrapper own image resolution and call ksync as the deploy engine (ADR 20260616-image-override).
- `internal/config` — ksync.yaml model: app list, `allowedContexts` allowlist (the safety model —
  the run targets the current-context or `--context`, but it must match an entry; entries are
  shell-style globs (`path.Match`, e.g. `k3s-*` for per-worktree microVMs), a plain name matches
  exactly; `SelectContext` enforces this, so one config can serve several interchangeable dev
  clusters yet never act on an unlisted one), optional top-level `imageLoad` (`command` + `allowParallel`, for separate-image-store
  clusters; `allowParallel` defaults true, set false for non-concurrency-safe loaders like
  `k3d image import`), per-app default namespace (ArgoCD destination.namespace parity), `needs`
  DAG, per-app `build` entries (image/context + optional name/dockerfile/watch/watchIgnore/command —
  `name` defaults to the image's last path segment, unique within an app, labels the image in the
  watch confirmation prompt); `SortByNeeds`.
- `internal/build` — source→image: docker build (or the `command` escape hatch producing
  `$KSYNC_IMAGE`), content-addressed dev tags `ksync-<12 hex of image ID>` (no persisted
  build state; `--provenance=false` keeps IDs deterministic), `.dockerignore`-scoped watch
  derivation (WatchScope), and `Loader` (the `imageLoad` hook: runs the command per build batch
  with `$KSYNC_IMAGES` set, for k3d/kind `image import` / registry push; serializes its calls
  unless `Parallel`, since k3d image import is not concurrency-safe). See ADRs
  20260612-build-integration, 20260613-image-load-hook, and 20260615-imageload-concurrency.
- `internal/render` — in-process kustomize (krusty) replicating
  `kustomize build --enable-helm --load-restrictor LoadRestrictionsNone`; byte-parity with the
  binary is enforced by test. `SetImages` (the build-tag injection path) also rewrites an
  explicit `imagePullPolicy: Always` to `IfNotPresent` for images ksync builds — the
  content-addressed local tag exists in no registry, so `Always` would force a doomed pull.
- `internal/ui` — human-facing output: a `logr.LogSink` that renders clean, colored,
  single-line records (a quiet variant drops Info/V noise; used for the engine and the routed
  klog/client-go stream), color helpers (NO_COLOR + TTY aware), and the live terminal block
  (`console`) — a set of per-app `Pipeline`s, each grouping its named **🔨 Build → 📦 Import →
  🚢 Deploy** stages (not-yet-reached stages shown as pending `○`; a deploy-only app collapses to
  one line) with a pinned Summary footer, height-clamped so concurrent groups never overflow.
  Each build/import command's output is collapsed to one live line per stage; the full log shows
  only on failure. On commit a build app **freezes its whole stage tree** (every build/import/deploy
  row keeps its own final time) instead of collapsing to the deploy line, so per-stage timings
  survive the run; a build-less app commits one `🚢 <app>  N applied  <time>` line (no redundant
  "Deploy" word — the committed deploy time is the deploy stage's own, via `ui.CommitInfo`). Off a
  terminal the block is inert (plain per-stage and per-app lines). Also the **watch confirmation
  picker** (`prompt.go`): a two-step interactive gate (single-select Build all / Select which to
  build / Skip, then an arrow-key + spacebar multi-select), a pure `buildPrompt` model (unit-tested
  via key events) behind a thin raw-mode driver (`ConfirmBuilds`) that runs only while the loop is
  idle. See ADRs 20260615-grouped-pipeline-progress, 20260616-committed-stage-timings, and
  20260616-manual-build-gate.
- `internal/watch` — dirty-set mapping (changed path → affected apps/build entries, with
  per-entry ignore predicates), dependency-root derivation (escaping
  chartHome/resources/values), recursive fsnotify watcher with per-root directory pruning.
- `internal/schedule` — pure scheduling state machine: debounce/coalesce, per-app
  serialization, bounded parallelism, needs gating, exponential retry backoff, and an optional
  external gate (`SetExternalBlock`) the loop uses to hold a deploy until its image has built. The
  watch loop runs two instances — needs-free builds and needs-gated deploys (ADR
  20260616-eager-build-ahead).
- `internal/engine` — gitops-engine wrapper (pin: argo-cd release-tag commits; k8s.io/* follow
  the engine's version): warm cluster cache, SSA, tracking-label-scoped prune, namespace
  auto-creation (create-if-missing — the app's own *and* every other namespace its resources
  reference, the latter bare/untracked so prune never touches it; ADR
  20260614-ensure-referenced-namespaces). After apply, a **health gate** blocks until every
  non-hook resource is Healthy (or `--timeout`), so a completed `Sync` means deployed-and-healthy
  and a `needs` edge waits for the dependency to actually serve (ADR 20260614-sync-health-gate). A
  no-diff sync skips hooks, **except** a currently-Degraded hook (re-run so a transiently-failed
  PostSync Job self-heals) or `--force` (re-run every hook, ArgoCD manual-sync parity; ADR
  20260616-hook-rerun-on-failure).
- `internal/loop` — the watch-mode event loop tying the above together; cluster and docker
  sides injected as SyncFunc/BuildFunc so it tests without either. Two scheduler instances run the
  two phases independently: an image builds the moment its source is dirty (needs-free), overlapping
  the dependency chain's deploys, while the deploy stays `needs`-gated and waits on the external gate
  until its own build finishes — so an unbuilt tag is never deployed, and `--max-parallel` now bounds
  builds and deploys with independent budgets (ADR 20260616-eager-build-ahead). An entry in
  `Options.Overrides` (a supplied image ref) is never built and its sources are not watched — the ref
  is injected at deploy (ADR 20260616-image-override). Manifest-only edits
  never invoke docker. By default (interactive TTY, no `-auto`) incremental changes pass through a
  **manual gate** (`Options.Gate`): instead of scheduling, they accumulate into a pending set and,
  once idle, the loop asks which images/apps to rebuild via the `internal/ui` picker, acting only on
  the returned `Decision` — startup convergence is ungated; the gate intercepts only `watcher.Events`
  (ADR 20260616-manual-build-gate). On settling back to idle after doing work — the initial
  convergence and every later rebuild batch — an `Options.OnIdle(took)` hook fires once; the command
  layer (`watchReporter`) turns it into a committed per-batch **Summary** and a `finished, watching
  for changes` log line (a change merely held/skipped by the gate runs no work, so it never fires).
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
