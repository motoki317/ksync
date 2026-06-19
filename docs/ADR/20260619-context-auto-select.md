---
date: "2026-06-19"
author: "motoki317"
status: "accepted"
---

# Context

ksync targeted the kubeconfig's current-context by default, then enforced `allowedContexts`
(`--context` overrode the default; either had to match an entry). The allowlist was the safety
gate; current-context was merely the convenient default selector.

That default has two flaws. First, it binds a ksync run to host-global state: the current-context
lives in `~/.kube/config` (or whatever `KUBECONFIG` resolves to) and is shared by every shell on
the machine. A run's target then depends on invisible external state, and the obvious way to make
`ksync sync` "just work" without `--context` — having ksync call `use-context` — would mutate that
shared state and silently retarget every other shell's `kubectl`. Second, it is friction for the
overwhelmingly common case: a dev whose `ksync.yaml` names exactly one local cluster still had to
keep their current-context pointed at it, or pass `--context` every time, even though there was
only ever one allowed answer.

# Decision

Drop the current-context default. `SelectContext` resolves the target from config alone:

- **`--context` given** — must match an `allowedContexts` entry, else refused. Unchanged.
- **No override, allowlist names exactly one *concrete* context** — target it automatically.
- **Otherwise** — ≥2 entries, or a single glob (`k3s-*`) with no concrete name — the target is
  ambiguous, so fail closed with an error telling the user to pass `--context`.

"Concrete" means free of `path.Match` metacharacters (`*?[`); a lone glob is ambiguous because it
names a family, not one cluster. ksync no longer reads the current-context at all
(`engine.CurrentContext` is removed).

# Consequences

The common single-cluster config needs neither a correctly-set current-context nor a `--context`
flag — `ksync sync` targets the one allowed cluster. ksync's target no longer depends on, and can
never mutate, the host-global current-context, so it cannot retarget other shells. The selection is
fully determined by the committed `ksync.yaml` plus an explicit flag — reproducible across machines
regardless of local kubectl state. Fail-closed on ambiguity keeps the safety property: ksync never
guesses which of several clusters to act on.

# Impact

`SelectContext` loses its `current` parameter; `resolveContext` stops calling
`engine.CurrentContext` (now deleted). Behavior change for multi-context / glob configs: a bare
`ksync sync` that previously rode the current-context now errors until `--context` is supplied —
including the documented `$KSYNC_CONTEXT` per-cluster `imageLoad` example and any per-worktree
`k3s-*` setup. This is the intended trade: those configs were the ambiguous case the current-context
was silently resolving, and resolving it via shared host state is exactly what this removes. The
apply path is untouched — `RESTConfig` already targets the resolved context via an in-memory
`ConfigOverrides`, never a kubeconfig write, so it never wrote current-context in the first place.

# Alternatives

- **Keep current-context as a fallback for the ambiguous case.** Adds the auto-select convenience
  without breaking multi-context configs, but retains the dependency on shared host state for
  exactly those configs and contradicts "fail closed when unsure" — rejected.
- **Auto-select any single entry, including a glob.** Would feed `k3s-*` to the cluster client as a
  literal context name and fail confusingly; a glob has no single target by definition.
- **Require `--context` always.** Maximally explicit and safe, but pure friction for the single
  -cluster majority, which the allowlist already disambiguates unambiguously.
