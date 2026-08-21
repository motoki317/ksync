# Agent guide

Compact navigation aid for AI agents working on this repo — the "where do I look" sheet. Humans
see [README.md](README.md) and the CLI's own help (`ksync help`, `ksync help <topic>`, `ksync
<command> -h` — the self-contained user guide; [docs/usage.md](docs/usage.md) is only a pointer into
it); design rationale lives in ADRs ([docs/ADR/](docs/ADR/)). Keep this file scannable — push detail
to those.

## What ksync is

A local-development sync loop for Kubernetes: a long-running CLI that watches local
`kustomization.yaml` directories and, on change, renders, diffs, and applies the affected app to
a local cluster — with ArgoCD-parity sync semantics and a fast change→applied loop
(**target: p50 ≤ 2s, p95 ≤ 5s for a single-app edit**, excluding hook Job runtime). Per-app
`build` entries extend the loop to source changes: docker build → content-addressed dev tag →
in-process `images:` injection → sync (M2, ADR 20260612-build-integration).

The tiebreaker when design or docs choices conflict: **easy for human users, straightforward,
no bloated docs**. Bloat means redundancy (saying one thing many times), not depth a user needs —
fix the redundancy rather than delete reference content, keep `ksync <cmd> -h` self-sufficient
and let docs defer flag detail to it, and surface a lean-vs-comprehensive doc fork to the user
instead of deciding it silently.

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

- `cmd/ksync/main.go` — the command bodies (`watch / sync / render / images / destroy / diff` and
  their `runX` pipelines) and `main`'s error→exit-code mapping. The **cobra** command tree, flags,
  and groups live in `cli.go`; the CLI help prose — each command's Long/Example plus the `ksync help
  <topic>` concept pages (config / builds / strategy / hooks / troubleshooting) that make the CLI its
  own user guide — lives in `help.go`. Flags are GNU `--long` (pflag) with `-f`/`-v` shorthands;
  `runX` bodies read pointer-bound flags so the framework swap changed parsing and help, not command
  logic (ADR 20260701-cli-cobra-and-help-as-docs). `signalContext` is the shared interrupt handler
  all entry points use: the
  first Ctrl-C cancels the context (graceful), the second `os.Exit(130)`s — so a non-context-aware
  step (engine.New's warm-cache LIST, render) can always be force-quit instead of swallowing every
  signal until it returns (ADR 20260627-double-signal-force-quit). `diff.go` is the **`ksync diff`**
  command: the read-only preview of `sync` —
  per-resource unified YAML diff against live, build-tag carry-forward, secret masking (ADR
  20260625-diff-command). `diff` and `sync`/`watch` default to a **server-side dry-run diff** (the
  apiserver's predicted post-apply object, so a field the cluster defaults or prunes is not seen as
  drift); `--client-diff` opts back into the in-process client-side diff (ADR
  20260625-server-side-diff-default). `images.go` is the **`ksync images`** command: renders the
  apps and prints the **canonical** refs (containerd-normalized via `distribution/reference`, so a
  consumer matches the cluster store by string equality) of the images they deploy, excluding
  `build:` repos (local dev tags, never pulled). `--live` also reads running-pod images so
  operator-derived ones absent from the manifests (an ECK Elasticsearch's data image from
  `spec.version`) are covered — the set a cache/pre-pull scopes to (ADR 20260617-images-command).
  `override.go` resolves **image overrides** (`--image IMAGE=REF` on `sync`, `diff`, and `watch`,
  or `KSYNC_IMAGE_OVERRIDES`): a supplied ref deploys a pre-built image instead of
  building that `build:` entry — the build (and its `imageLoad`) is skipped and the ref is injected
  at deploy; an override for an image no app builds is dropped with a note. This is what lets a
  wrapper own image resolution and call ksync as the deploy engine (ADR 20260616-image-override).
  On `watch` an override has **takeover** semantics: the entry is seeded from the supplied ref on
  first convergence (like `sync`) but its source stays watched, and the first source edit drops the
  override and rebuilds (ADR 20260702-watch-image-override-takeover).
- `internal/config` — ksync.yaml model: app list, `allowedContexts` allowlist (the safety model —
  ksync never reads the host current-context (a shared, host-global setting); `SelectContext`
  auto-targets the **sole** concrete entry, else (≥2 entries, or a single glob) requires `--context`
  and fails closed without it; an `--context` must still match an entry; entries are shell-style
  globs (`path.Match`, e.g. `k3s-*` for per-worktree microVMs — always needing `--context`), a plain
  name matches exactly; so one config can serve several interchangeable dev clusters yet never act on
  an unlisted one (ADR 20260619-context-auto-select)), optional top-level `imageLoad` (just `command`, for
  separate-image-store clusters; loads are always serialized and coalesced — see `internal/build`),
  per-app default namespace (ArgoCD destination.namespace parity), `needs`
  DAG, per-app `build` entries (image/context + optional name/dockerfile/watch/watchIgnore/command —
  `name` defaults to the image's last path segment, unique within an app, labels the image in the
  watch confirmation prompt); per-app `patches` (post-render JSON6902 patches — target by literal
  GVK+name(+ns), inline RFC6902 ops, op `value`s may use `${VAR}`; the DSL home for
  deploy-environment fields the kustomization can't carry, see render below and ADR
  20260623-post-render-patches); per-app `clientRender` (render this app the ArgoCD way — `helm
  template` with cluster capabilities but no server-side dry-run, so a chart that ships a CRD with
  custom resources of that kind renders before the CRD exists, instead of failing the default render's
  server-side mapping with "no matches for kind"; disables `helm lookup` for the app, render-only —
  diff/apply unchanged since the server-side diff already falls back per-resource; ADR
  20260630-client-render-per-app); `Config.Dir` (the `${KSYNC_WORKDIR}` anchor); `SortByNeeds`.
- `internal/build` — source→image: docker build (or the `command` escape hatch producing
  `$KSYNC_IMAGE`), content-addressed dev tags `ksync-<12 hex of image ID>` (no persisted
  build state; `--provenance=false` keeps IDs deterministic), `.dockerignore`-scoped watch
  derivation (WatchScope), and `Loader` (the `imageLoad` hook: runs the command with `$KSYNC_IMAGES`
  set, for k3d/kind `image import` / k3s `ctr import` / registry push; always serializes its calls
  since k3d image import is not concurrency-safe, and **coalesces** — images that finish while a load
  runs are batched into the next invocation, so a bulk-capable command amortizes its per-call cost).
  `BuildGroup` runs one bulk command for a group's dirty members (a `docker buildx bake`, a host
  compile producing many images); `GroupGate` **serializes a group's command across the concurrent
  per-app builds by default** — a bulk command need not be concurrency-safe (a cold `cargo zigbuild`
  races to create its shared wrapper cache → `File exists (os error 17)`, the same class of hazard the
  Loader serializes for), opt out per group with `parallel: true` (ADR 20260630-serialize-build-groups).
  See ADRs 20260612-build-integration, 20260613-image-load-hook, _20260615-imageload-concurrency,
  20260617-imageload-batching, and 20260630-serialize-build-groups.
- `internal/render` — in-process kustomize (krusty) replicating
  `kustomize build --enable-helm --load-restrictor LoadRestrictionsNone`; byte-parity with the
  binary is enforced by test. `SetImages` (the build-tag injection path) also rewrites an
  explicit `imagePullPolicy: Always` to `IfNotPresent` for images ksync builds — the
  content-addressed local tag exists in no registry, so `Always` would force a doomed pull.
  `ApplyPatches` is the post-render patch path (sibling of SetImages, applied **before** it in
  render/sync/watch/images): each `config.Patch` matches **exactly one** rendered object by literal
  GVK+name(+ns) — fail-closed on 0/≥2, not kustomize's regex `Selector` — and applies its inline
  RFC6902 ops via a single-resource `patchjson6902.Filter`, refreshing `Objects` so `YAML()` and
  `Objects` never drift. `${VAR}` in op `value`s is expanded by a narrow custom scanner (`${NAME}`,
  `${NAME:-default}`, and `$$`→`$` only; a bare `${NAME}` is fail-closed on undefined, while the
  `:-default` form is the explicit opt-out — POSIX colon semantics, so the literal default applies
  when the variable is unset **or** empty, and `${FOO:-}` yields `""` for an absent var, matching a
  helmfile `env ""` default without breaking fail-closed) over the process env plus the built-in
  `${KSYNC_WORKDIR}` = config dir; expansion touches only `value` strings (`NewVarLookup` builds the
  resolver). Because ksync runs next to the cluster, `${HOME}` self-resolves to the cluster host's
  home, so a wrapper needs no per-environment plumbing (ADRs 20260623-post-render-patches,
  20260629-default-var-expansion). `Render(dir, clientRender)` picks the helm command per app:
  `Options.HelmCommand` (the default server-side dry-run / `lookup` wrapper) or
  `Options.ClientRenderCommand` (the no-lookup wrapper an app's `clientRender` selects); `cmd/ksync`
  (`helmlookup.go`) writes both wrappers as separate files — sharing one set of discovered
  capabilities, the choice made by which path kustomize is handed, never a mutable env var that
  concurrent renders would race (ADR 20260630-client-render-per-app).
- `internal/ui` — human-facing output: a `logr.LogSink` that renders clean, colored,
  single-line records (a quiet variant drops Info/V noise; used for the engine and the routed
  klog/client-go stream), color helpers (NO_COLOR + TTY aware), and the live terminal block
  (`console`) — a set of per-app `Pipeline`s, each grouping its named **🔨 Build → 📦 Import →
  🚢 Deploy** stages under a header that carries the app name and the **group's total wall-clock**
  (the span across its stages, not the sum — builds overlap), not-yet-reached stages shown as
  pending `○`, with a pinned Summary footer, height-clamped so concurrent groups never overflow. A
  **deploy-only** pipeline collapses to one line — an app with no builds, *or* a build app whose
  images were all supplied as overrides so it never built (collapses once its deploy starts, having
  no build/import rows to come). Each build/import command's output is collapsed to one live line
  per stage; the full log shows only on failure. On commit a build app **freezes its whole stage
  tree** (every build/import/deploy row keeps its own final time, the header its group total)
  instead of collapsing, so per-stage timings survive the run; a deploy-only app commits one
  `🚢 Deploy <app>  N applied  <time>` line (the "Deploy" word matches the in-tree deploy row; the
  committed deploy time is the deploy stage's own, via `ui.CommitInfo`). `ui.DeployLine`'s `kind`
  arg carries that word — sync passes "Deploy", `ksync destroy` passes "" (it is not a deploy). **Off
  a terminal the block is inert; instead each build streams its command output live, line by line,
  prefixed `app/label <phase> [<elapsed>] │ ` (the docker-buildx model, `#12 45.3s text` — the log
  itself is the liveness signal, no heartbeat, and the per-line running elapsed, bracketed left of the
  bar so it is never mistaken for build output, shows how long a stage has run at a glance)**; imports
  buffer and report a result line (a coalesced load tees one command to every
  waiting app, so per-stage streaming would duplicate it — dumped only on failure); a deploy streams a
  health-gate line only when its not-ready status changes; the run ends with the slowest-first
  `Timings` recap after the Summary (ADR 20260701-non-terminal-log-streaming). The console also owns
  **inter-section spacing**: every committed write is tagged with a `ui.Section` (Log/Plan/Summary/
  Pipeline) and the console emits exactly one blank line at each kind change — so a new kind of output
  is separated automatically and producers carry no hand-rolled blanks (ADR
  20260619-section-output-spacing). Also the **watch confirmation
  picker** (`prompt.go`): a single-view interactive gate — a **Rebuild all** master checkbox
  (cursor default, selected) over one spacebar-toggle row per change; Enter on the default rebuilds
  all (one keystroke), toggling an item narrows to just that subset (the master turns off; items
  render dim/implied while it is on), and an empty selection skips (no Skip row — clear the master,
  then Enter). A pure `buildPrompt` model (unit-tested via key events) sits behind a thin raw-mode
  driver (`ConfirmBuilds`) that runs only while the loop is idle. The driver **polls** the terminal
  non-blocking (`pollReader`; no tty supports a read deadline) so it reacts between keystrokes: an
  `abort` channel lets the loop **refresh an open prompt** when fresh changes land — it tears down and
  re-asks with the larger pending set (the picker reports `aborted`, the gate a `Decision{Reask}`) — and
  ctx/SIGTERM is honored within a poll tick, not only on the next key. See ADRs
  20260615-grouped-pipeline-progress, 20260616-committed-stage-timings, 20260616-manual-build-gate,
  20260618-group-total-and-deploy-line, 20260619-single-view-build-picker,
  20260619-refresh-open-build-prompt, and 20260701-non-terminal-log-streaming.
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
  20260614-ensure-referenced-namespaces). `Sync` is a **convergence loop**: it retries every
  apply/hook failure — unclassified — until the app is applied-and-healthy or `--timeout` expires,
  rather than failing the run on the first error, so a fresh-cluster CRD+CR registration race (the
  next loop iteration's fresh kubectl factory re-resolves the just-registered kind, client-go
  `restmapper` `Fresh()`→`Reset()` self-heal) or a webhook not yet serving self-heals. The lever:
  a carried `SyncFailed`/failed-hook result short-circuits gitops-engine's next operation straight
  back to Failed, so `Sync` **strips the completed-unsuccessful results** (`stripFailedResults`,
  keeping succeeded resources and completed hooks) before re-seeding `WithInitialState` — the
  stripped tasks re-run, the rest do not. The loop also **re-fills default namespaces each cycle**:
  a namespaced CR whose CRD is still registering reads as cluster-scoped at Sync start (`IsNamespaced`
  can only classify served kinds), so its target keeps an empty namespace and the health gate — which
  keys by namespace — reads the (converged) resource `Missing` forever; the per-cycle re-fill matches
  the target key to the live object once the CRD is served (idempotent, so a no-op on the clean path).
  Backoff 1s→30s, reset when the failing set shrinks;
  `--timeout` is the only bound (no retry-limit knob). Each failure/retry/recovery is reported via
  `OnRetry` (UI-neutral, like `OnWait`) as a distinct committed line in both TTY and non-TTY output
  (`internal/ui` `Stage.Event`/`SetTailQuiet`, deduped by message), and `TimeoutError` names the
  retry count + last failure (ADR 20260709-sync-convergence-retry). After apply, a **health gate**
  blocks until every non-hook resource is Healthy (or `--timeout`), so a completed `Sync` means
  deployed-and-healthy and a `needs` edge waits for the dependency to actually serve (ADR
  20260614-sync-health-gate); a target still Missing after a successful apply re-enters the apply
  path (D6). While waiting, `OnWait` reports the not-yet-ready resources (`[]ResourceStatus`,
  UI-neutral) so the live deploy line can name them. The health-gate/apply loops split `ctx.Err()`: only the app's own
  `--timeout` (`DeadlineExceeded`) is a typed `TimeoutError`; a Ctrl-C or a sibling app's failure
  aborting the run (`Canceled`) returns the cancellation, which the command maps to `ksync:
  interrupted` + exit 130 (not the raw error). `Diagnose` (`diagnose.go`) gathers a **bounded,
  actionable** dump on a real timeout, **and on a user Ctrl-C of a one-shot `sync`** (the usual way to
  abandon a wedged deploy — `cmd`'s `reportSyncDiagnostics`/`shouldDiagnose`, gated on `userInterrupted`
  = the **signal** ctx being cancelled, *not* the app's own ctx; a sibling-abort cancels this app too
  but `userInterrupted` is false then, so the aborted-but-progressing app stays quiet while the failed
  sibling reports its own error). `Diagnose` self-selects the still-unhealthy resources, so an app
  merely mid-rollout prints nothing, and `watch` skips the dump on its routine Ctrl-C quit (ADR
  20260627-actionable-diagnostics). The gather runs on a **fresh `context.Background()`** (never the
  just-cancelled sync ctx, which returned only `context canceled`), so a second Ctrl-C still
  force-quits before it finishes: each unhealthy resource plus the related pods (found via the cache's
  `IterateHierarchyV2` ownership walk). The dump is kept terse — events filtered to **warnings** then
  deduped (per reason, the most-informative kept; the redundant `BackOff` dropped), identical replica
  pods **collapsed** to one representative `+N`; both bounds name what they drop, never silently —
  distinct modes past the display cap as `+N more distinct failure modes`, related pods past the
  `diagMaxPodScan` work cap (each costs a live get/event-list/log-stream) as `+N related pods not
  inspected`; one log stream per pod (previous run for a crashed container, kubelet placeholder
  filtered), and a container's **last-termination** appended to its waiting state so a CrashLoopBackOff
  names its real exit code / OOMKill (ADRs 20260623-sync-timeout-diagnostics,
  20260627-actionable-diagnostics; live fixtures in `testdata/diagnostics/`). A
  no-diff sync skips hooks, **except** a currently-Degraded hook (re-run so a transiently-failed
  PostSync Job self-heals) or `--force` (re-run every hook, ArgoCD manual-sync parity; ADR
  20260616-hook-rerun-on-failure). `serverdiff.go` is the **diff strategy** shared by `Diff` and
  `Sync`'s apply-skip: server-side by default (gitops-engine's `WithServerSideDiff` over
  `kube.ManageServerSideDiffDryRuns` — the JSON-emitting dry-run applier, **not** `ManageResources`,
  whose status-line printer silently degrades every resource to client-side — plus the warm cache's
  `GetGVKParser`), with a per-resource fallback to client-side on a dry-run error; `Sync` runs it on
  the first reconcile only (the apply-set decision), client-side on the health-wait polls. A field the
  apiserver defaults or prunes (a `maxUnavailable` behind a disabled feature gate) is therefore not
  re-applied every sync. `--client-diff` opts out (ADR 20260625-server-side-diff-default).
- `internal/loop` — the watch-mode event loop tying the above together; cluster and docker
  sides injected as SyncFunc/BuildFunc so it tests without either. Two scheduler instances run the
  two phases independently: an image builds the moment its source is dirty (needs-free), overlapping
  the dependency chain's deploys, while the deploy stays `needs`-gated and waits on the external gate
  until its own build finishes — so an unbuilt tag is never deployed, and `--max-parallel` now bounds
  builds and deploys with independent budgets (ADR 20260616-eager-build-ahead). An entry in
  `Options.Overrides` (a supplied image ref) is deployed as-is instead of built (ADR
  20260616-image-override). Under `sync` the override holds for the whole run — the entry is never
  built and its sources are not watched; under `watch` it has **takeover** semantics — seeded on
  first convergence but its source stays watched, and the first source edit drops it and rebuilds
  (ADR 20260702-watch-image-override-takeover).
  Manifest-only edits
  never invoke docker. By default (interactive TTY, no `-auto`) incremental changes pass through a
  **manual gate** (`Options.Gate`): instead of scheduling, they accumulate into a pending set and,
  once idle, the loop asks which images/apps to rebuild via the `internal/ui` picker, acting only on
  the returned `Decision` — startup convergence is ungated; the gate intercepts only `watcher.Events`
  (ADR 20260616-manual-build-gate). A change arriving **while a prompt is open** folds in: the loop
  flags it, and once the burst settles calls `Gate.Abort` to refresh the prompt — the picker re-asks
  with the full pending set (`Decision{Reask}`) so the user sees every edited app, not just those
  dirty when it opened (ADR 20260619-refresh-open-build-prompt). On settling back to idle after doing
  work — the initial
  convergence and every later rebuild batch — an `Options.OnIdle(took)` hook fires once; the command
  layer (`watchReporter`) turns it into a committed per-batch **Summary** and a `finished, watching
  for changes` log line (a change merely held/skipped by the gate runs no work, so it never fires).
- `internal/leakcheck` — the no-leak guard (see Conventions).
- `docs/ADR/` — dated decision records (`YYYYMMDD-title.md`, template at `_template.md`). The
  research context behind the whole project is tracked here too:
  [why ksync exists / build-vs-buy](docs/ADR/20260612-build-vs-buy-tool-landscape.md) and the
  [verified ArgoCD semantic contract](docs/argocd-parity.md). Read those before design work; do
  not re-litigate tool selection without new evidence.
- `docs/plans/` — gitignored single-session scratch.
- `HANDOFF.md` — **gitignored, local-only**, and no longer load-bearing: if present, it holds only
  private reference-environment details (specific cluster/repo names). Never copy its private
  references into tracked files.

## Build / test

```bash
just build       # CGO_ENABLED=0 go build → ./ksync  (the authority)
just test        # go test ./... (includes internal/leakcheck)
just test-race   # go test -race ./... — run yourself before pushing concurrency changes
just check       # gofmt gate + go vet + (advisory) golangci-lint — run before committing Go
just leakcheck   # the no-leak guard alone
```

The race detector is deliberately **CI-only** (`ci.yaml` runs `just test-race`; the local gates —
`just pre-commit`, `just test` — run plain `go test` to keep commits fast). The blind spot: a
`-race`-only failure is invisible locally — build, tests, and the commit hook all stay green, and
only the push fails CI. Before pushing anything touching goroutines, channels, `sync.*`, or
scheduling, run `just test-race`; for a non-deterministic race, loop the suspect test
(`go test -race -run TestX -count=500 ./...`) — a single pass proves little.

The flake devShell (`nix develop` / direnv) installs commit-time git hooks: every commit runs
`just pre-commit` (build, gofmt, vet, go test — each commit must compile and pass), and a second
hook runs `just nix-build` ONLY when the commit touches `go.mod`/`go.sum`/`flake.nix`/`flake.lock`
— the only changes that can break the Nix path (e.g. a stale `vendorHash`; sound because the
flake sets `proxyVendor`, pinning `vendorHash` to go.mod/go.sum alone). There is no pre-push
hook; the guarantee lives at commit time so non-building commits never land in history.

## Releases

One track: pushing a `vX.Y.Z` git tag publishes the release. The runbook (gates, order, traps) is
the `release` skill (`.claude/skills/release/SKILL.md`) — this is the summary.

- The tag fires **two** workflows, both must go green: `release.yaml` (GoReleaser, `.goreleaser.yaml`)
  builds the GitHub Release (linux/darwin × amd64/arm64 tarballs + grouped changelog);
  `cachix.yaml` builds `.#ksync` on three runners and pushes to the `motoki317-ksync` cache so
  `nix run github:motoki317/ksync` substitutes the binary.
- **No version file to bump.** `main.version` defaults to `dev` and is stamped at build time —
  GoReleaser from the tag (`-X main.version={{.Version}}`), the flake from `self.shortRev`. The tag
  *is* the version; there is no source constant to edit.
- A consequence of the two stampers: the GoReleaser binary's `ksync version` prints `X.Y.Z`, while
  a `nix run github:motoki317/ksync/vX.Y.Z` build prints the git short-rev. Known, not a bug.
- Changelog excludes (`docs`/`test`/`chore`/`ci`) are scope-aware (`^docs(\(.+\))?!?:`) so a scoped
  `docs(readme):` is dropped like an unscoped `docs:`; the rest is grouped Features/Bug fixes/
  Performance/Other. On the first tag (no previous tag) GoReleaser falls back from `use: github` to
  `git` automatically.

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
- ADRs are dated `YYYYMMDD-title.md`; design rationale lives there, not in comments. A **fully
  superseded** ADR is renamed with a leading `_` (`_YYYYMMDD-title.md`), set to `status:
  superseded`, and opens with a "Superseded by X" note; a **partially** superseded one keeps its
  undated `_template.md` is the ADR template, not a superseded record.)
- Code comments explain WHY (non-obvious decisions, hidden constraints) — never WHAT. When
  cutting comments, the bar is **deletion, not shortening**: delete anything a competent reader
  can infer from the code, names, keys, or values; only non-inferrable WHY survives.
- TDD for pure logic (render orchestration, dirty-set mapping, scheduling); fixture-driven where
  possible.

## Where durable state lives (docs layout)

Long-lived, shareable context must be **git-tracked** — but anything referencing the private
reference environment stays in gitignored local files:

- **`docs/ADR/`** (tracked) — dated decision records, including the project's research context
  ([build-vs-buy](docs/ADR/20260612-build-vs-buy-tool-landscape.md)) and
  [`docs/argocd-parity.md`](docs/argocd-parity.md) (the verified semantic contract).
- **git log** (tracked) — the authoritative per-change "what + why".
- **`HANDOFF.md`** (gitignored) — only private reference-environment details; local-only,
  not load-bearing.
- **`docs/plans/`** (gitignored) — volatile single-session scratch.
