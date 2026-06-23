---
date: "2026-06-23"
author: "motoki317"
status: "accepted"
---

# Context

Some rendered fields must differ per deploy environment in a way the committed kustomization cannot
express. The motivating case: a wrapper runs ksync against two local clusters — a microVM where
ksync runs inside the VM (`$HOME=/root`, the worktree mounted at a fixed path) and Docker Desktop
where ksync runs on the host (`$HOME=/Users/<user>`, the worktree at the real checkout). A pod
`hostPath` for a host-shared cache has to resolve to `$HOME/.cache/<app>` and a secrets mount to
`<worktree>/secrets` — different absolute paths per environment, and one (`/Users/<user>/…`) is
user-specific so it cannot be hardcoded. The previous helmfile-based flow injected these with
helmfile `set:` (`requiredEnv "HOME"`, `exec "pwd"`); the kustomize/ksync migration hardcoded them
to the microVM paths, which breaks Docker Desktop.

Constraints that shaped the solution:

- **The kustomization must stay a pure kustomize file** — no ksync-specific placeholder tokens in
  `kustomization.yaml`/values. Environment knowledge belongs in ksync's own DSL (`ksync.yaml`), the
  way image overrides already live outside the manifests.
- **ksync must not grow a templating engine.** It renders kustomize and applies; the existing
  post-render mutation (`SetImages`) is narrow and mechanical. A patch facility must stay equally
  small, or it becomes a second, worse kustomize.

# Decision

Add a per-app `patches:` field to `ksync.yaml`: post-render **RFC 6902 (JSON patch)** operations
ksync applies to an app's rendered objects, between render and image injection. Each entry is a
`target` (literal GVK + name, optionally a namespace) and an inline `patch` (a list of ops). This is
the post-render sibling of `SetImages`.

Targeting is **literal and fail-closed**. The target matches a rendered object by exact-string
GVK + name (and namespace when set) — not kustomize's `Selector`, whose regex matching would mishandle
a dotted group like `networking.k8s.io`. A patch must match **exactly one** object; zero or several is
an error. Matching is against the rendered `metadata.namespace`, not an app-level default namespace
(which is applied later, at sync time).

A patch's op **`value` strings** may contain `${VAR}` references, expanded at sync time from the
process environment plus the built-in `${KSYNC_WORKDIR}` (the config file's directory). Expansion is
deliberately minimal: the grammar is exactly `${NAME}` and `$$`→`$`; an undefined variable fails the
run (fail-closed, like `requiredEnv`); expansion touches **only** `value` strings, never `path`,
`from`, or `op`, so a variable can never reshape the patch or a JSON pointer. ksync expands whatever
variable is named and assigns it no meaning — the caller decides which variables exist and what they
stand for, keeping environment semantics out of ksync.

This works for the motivating case with **no plumbing in the wrapper**: ksync runs next to the
cluster, so its own `$HOME` is already the cluster host's home (the VM's, or the developer's), exactly
as `requiredEnv "HOME"` resolved before. Ordering is `render → patches → SetImages → sync`: patches
guard the committed manifest shape, while image injection must win on the built dev tag and the
`imagePullPolicy: Always→IfNotPresent` fix. `ApplyPatches` mutates the resMap and refreshes the object
slice together, so `Result.YAML()` and `Result.Objects` never drift. It runs in every render-consuming
command — `render` (so its output is what sync deploys), `sync`, `watch`, and `images`.

Structure is validated at config load (target has kind+name; the patch parses as a non-empty list of
well-formed ops; pointers look like pointers). Variable resolution and target matching are checked at
sync, since they need the runtime environment and the rendered objects — so an undefined variable or a
zero/multi match fails only the run that actually renders that app, never an unrelated `sync app-b`.

# Consequences

- Environment-specific fields resolve per run while the kustomization stays pure kustomize and the
  wrapper does nothing special — the value the helmfile `set:` flow gave, without helmfile.
- The patch is committed and declarative in `ksync.yaml`; there is no generated overlay to keep in
  sync and no "bare `ksync` breaks without the wrapper" failure mode.
- The blast radius is bounded: literal exact-one targeting plus a fail-closed `test` op guard mean a
  patch either applies to the one intended object or fails the run — it cannot silently mis-apply.
- `ksync render` output is now environment-dependent when a patch uses `${VAR}` — intended for a
  local-dev inner-loop tool (it shows what *this* environment deploys), but worth knowing.
- The committed patch addresses rendered objects by JSON pointer (array indices), so a chart upgrade
  that reorders fields can require re-deriving a pointer; the `test` op turns that into a clean
  failure rather than a silent mis-patch.

# Impact

- `internal/config`: `App.Patches []Patch` with `PatchTarget` and structural validation; `Config.Dir`
  exposes the config directory for `${KSYNC_WORKDIR}`.
- `internal/render`: `Result.ApplyPatches` (literal match, value-only `${VAR}` expansion via a custom
  scanner, single-resource `patchjson6902.Filter`, Objects refresh) and `NewVarLookup`.
- `cmd/ksync` + `internal/loop`: apply patches after render, before `SetImages`, in `render`, `sync`,
  `images`, and the watch loop; the loop carries the config dir as `Options.WorkDir`.
- Tests: render apply/expand/match/fail-closed cases; config patch validation.

# Alternatives

- **`${VAR}` tokens in the kustomization values.** Rejected: it makes `kustomization.yaml` a
  non-kustomize file. ksync-specific behavior belongs in `ksync.yaml`.
- **A caller-generated patch file passed by env/flag, ksync env-agnostic.** Viable, and it keeps env
  resolution wholly in the wrapper, but it needs the wrapper to resolve and write a patch per run, and
  a bare `ksync` invocation (no wrapper) loses the patch. Since ksync's runtime `$HOME` already equals
  the cluster host's, committed `${VAR}` patches need zero wrapper plumbing — a strictly smaller moving
  part for this tool's job. The general value-injection mechanism can still be added later if a need for
  non-committed, caller-computed patches appears.
- **Runtime env substitution across all of `ksync.yaml`.** Rejected as scope creep — expansion is
  confined to patch `value` strings, with no defaults/conditionals/nesting, to stay a small feature
  rather than a templating layer.
- **kustomize's `Selector` / builtin `PatchJson6902Transformer`.** Rejected: the transformer is the
  deprecated `api/builtins` path and applies to every selector match (against the exact-one rule), and
  `Selector` matches IDs by regex. A literal target with an explicit exact-one check is predictable.

# Notes

`ApplyPatches` is atomic per object (a failed `test` op leaves that object untouched) but not across
the patch list: if an earlier patch mutated the result and a later one fails, the in-memory result is
partially patched. That is safe because every caller discards the result on error — render/sync abort
the app. The general value-injection / caller-supplied-patch-file path is intentionally deferred; this
ADR delivers the committed-in-`ksync.yaml` form the motivating case needs.
