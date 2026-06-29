---
date: "2026-06-29"
author: "motoki317"
status: "accepted"
---

# Context

Post-render patch `value` expansion (ADR 20260623-post-render-patches) is fail-closed: a `${VAR}`
whose variable is undefined aborts the run, mirroring helmfile `requiredEnv`. That is correct for a
value that *must* be present per environment (a host path, a worktree root).

It breaks an equally common case: an **optional** value the previous helmfile flow defaulted to empty
with `env "SOME_KEY" | default ""` — a third-party API key set in CI and staging but legitimately
absent in local dev. Injecting it with a bare `${SOME_KEY}` patch turns that absence into a hard
render failure, so the only fail-closed-safe options were to drop the injection (the rendered manifest
keeps the chart's own empty default, but the patch no longer round-trips the value when it *is* set) or
to require every developer to export the variable. Neither matches the helmfile parity the migration
targets.

ADR 20260623 explicitly deferred "defaults/conditionals/nesting" as scope creep. This ADR revisits
*only* defaults, because the empty-default case is exactly the optional-value parity gap above, not the
general templating layer that was rejected.

# Decision

Extend the patch-value scanner with one form: **`${NAME:-default}`**, POSIX `${parameter:-word}` colon
semantics. The literal `default` is substituted when `NAME` is unset **or** set-but-empty; otherwise
the variable's value is used. A bare `${NAME}` keeps its existing meaning — fail-closed on undefined,
empty string when set-but-empty.

The default is taken **literally**: it is not itself re-expanded, the first `}` ends the form (so a
nested `${...}` inside a default is unsupported), and only the colon form `:-` is recognized — there is
no unset-only `-`. This keeps the grammar a fixed, three-token set (`${NAME}`, `${NAME:-default}`,
`$$`→`$`) rather than the start of a shell-parameter-expansion implementation.

The motivating injection becomes `${SOME_KEY:-}`: present → the value, absent → `""`, no render
failure — the `env "SOME_KEY" | default ""` behavior, expressed in ksync's own DSL.

# Consequences

- An optional, environment-varying value injects via a committed patch and renders everywhere: its
  value where set, its default where not — no per-developer env export, no dropped injection.
- The fail-closed guarantee is unchanged for the form that wants it: a required `${VAR}` still aborts
  on absence. Defaulting is opt-in per reference, visible at the call site as `:-`.
- The grammar stays small and bounded; `:-` is the one addition, with no defaults/conditionals/nesting
  creep beyond it (an undefined bare `${VAR}` still fails, preserving ADR 20260623's intent for the
  required case).

# Impact

- `internal/render` (`expandVars`): parse `${...}` content as `name[:-default]` (`strings.Cut` on
  `:-`); apply the default when the lookup misses or yields empty. Doc comment updated.
- Tests: `TestExpandVars` table gains the `:-` cases plus regressions that bare `${NAME}` still
  fail-closes on unset and yields `""` on set-but-empty; an `ApplyPatches` case drives an unset var
  with a default through the full patch path.
- No config-schema or API change — the patch DSL surface is identical; only the value grammar widens.

# Alternatives

- **Keep fail-closed; require the env var or drop the injection.** Rejected: it either forces every
  local-dev environment to export an otherwise-irrelevant key, or loses the round-trip of the value
  when it *is* set — neither matches the helmfile `default ""` parity.
- **Default the value in the wrapper before invoking ksync.** Viable, but it pushes per-key knowledge
  back into the wrapper for a value that is otherwise fully described by the committed patch, and a bare
  `ksync` invocation would still fail-close. A patch-level default keeps the manifest self-contained.
- **Full POSIX parameter expansion (`-`, `:?`, `:+`, nesting, re-expanded word).** Rejected as the
  scope creep ADR 20260623 named. Only `:-` is needed for the optional-value case; the rest is grammar
  surface for no current use.

# Notes

`${NAME:-}` (empty default) is the canonical "optional, default empty" spelling and the one the
migration uses. Because the default is literal, `${A:-$$}` writes `$$`, not `$` — an accepted edge of
keeping the default unexpanded; no real patch relies on escapes inside a default.
