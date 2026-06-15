# Context allowlist: `context` → `allowedContexts`

Date: 2026-06-15
Status: accepted

## Context

ksync.yaml named exactly one kubectl context (`context: <name>`), and ksync only ever talked to
that context — never the kubeconfig's current-context. That was the whole context-safety model: a
config checked into a repo could not point a teammate's ksync at the wrong cluster, because the
target was a fixed string, not whatever happened to be current.

But one fixed context cannot describe a stack that is meant to run on **more than one
interchangeable dev cluster**. The motivating case: the same app set deploys to a shared-daemon
Docker Desktop *and* to a separate-image-store local k3s (a microVM) — two clusters a developer
switches between, neither of them production. With a single `context:` the developer had to edit
the config (and risk committing the edit) to move between them, and the image-load step could not
know which cluster was selected.

## Decision

Replace the single `context: <name>` with `allowedContexts: [<name>, …]` — an allowlist — and
resolve the *actual* target at run time.

- The run targets the kubeconfig's **current-context** by default, or an explicit `--context
  <name>` flag. Either way the chosen context **must be a member of `allowedContexts`**, or the
  run is refused with an error naming the allowed set. `config.SelectContext(override, current)` is
  the pure gate; `engine.CurrentContext()` reads the current-context for it.
- This **preserves the safety property** the old model had — ksync can still only ever act on a
  context the config explicitly lists, so a stray current-context pointing at production is
  rejected, not silently used — while letting one config serve several dev clusters. The allowlist
  is strictly more expressive than the single string (a one-element list is the old behavior, save
  that you now select it via current-context/`--context` rather than it being forced).
- The selected context is exported to build, build-group, and `imageLoad` commands as
  **`$KSYNC_CONTEXT`**, joining `$KSYNC_IMAGE(S)`. This is what lets one `imageLoad` branch per
  target — a no-op for a shared-daemon cluster, an import/push for a separate-store one — without
  ksync carrying any per-cluster knowledge.
- Each `allowedContexts` entry is a **shell-style glob** (`path.Match`: `*`, `?`, `[…]`); a name
  with no metacharacters matches exactly, so this is backward-compatible with literal lists. A glob
  lets one entry cover a family of clusters whose context names are not known up front — the
  motivating case is **per-worktree/per-branch microVMs** (`k3s-feature-a`, `k3s-feature-b`, …),
  several of which run at once, each with a distinct context name, all matched by `k3s-*` without
  editing the config per branch. Patterns are validated at parse time (a malformed glob is a config
  error, not a silent non-match). Globs only ever *widen* the allowlist, never narrow it, so the
  safety property is preserved exactly as long as patterns stay tight; `*` matches everything and
  defeats the gate, which is on the user (same as listing production explicitly would be).

## Consequences

- **Breaking config change**: `context: x` becomes `allowedContexts: [x]`. There is no
  compatibility shim — the field is renamed outright (ksync is pre-1.0 and the schema is validated,
  so a stale `context:` fails loudly via `UnmarshalStrict`).
- A `--context` flag now exists on `sync`/`watch`/`render`/`destroy`. Default (unset) keeps the
  zero-config ergonomics: just `kubectl config use-context` and run.
- `imageLoad` gains the ability to be cluster-aware via `$KSYNC_CONTEXT`, which is what makes a
  single config deployable to both a daemon-shared and a separate-store cluster.

## Impact

- `internal/config`: `Context string` → `AllowedContexts []string`; new `SelectContext`
  (glob-matches each entry via `path.Match`); validation requires ≥1 non-empty entry and rejects a
  malformed glob.
- `internal/engine`: new `CurrentContext()`; `RESTConfig`'s "never current-context" comment updated
  (the caller now resolves and validates the context before calling).
- `internal/build`: `Builder`/`Loader` gain `KubeContext`, exported as `$KSYNC_CONTEXT`.
- `cmd/ksync`: each subcommand registers `--context`, resolves once via `resolveContext`, and threads
  the result everywhere the single `cfg.Context` was used (engine, render, plan, build).

## Alternatives

- **Keep one `context:`, add a sibling `--context` override.** Smaller change, but an unconstrained
  override re-opens the "point at the wrong cluster" hole the fixed string closed; the allowlist is
  what keeps the override safe.
- **Use current-context implicitly with no allowlist.** Most ergonomic, but throws away the entire
  safety model — exactly what the original single-context design existed to prevent.
- **Per-context config blocks** (`contexts: {docker-desktop: {...}, k3d: {...}}`). Handles clusters
  that differ in more than identity, but the dev clusters here are interchangeable; an allowlist
  plus `$KSYNC_CONTEXT` for the one thing that differs (image load) is far less config for the same
  result. Revisit if clusters ever need to differ in app set or namespaces.
