---
date: "2026-09-25"
author: "motoki317"
status: "accepted"
---

# Context

Optional apps need the same config as the core stack to retain cross-app `needs`
ordering. Positional app names select an exact set, but cannot declare reusable
groups such as debug tools or metrics.

# Decision

An app can declare `profiles: [debug]`. Profile names match
`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$` and cannot repeat within an app.

Without positional names, every command selects unprofiled apps plus apps with any
active profile, in declaration order. `--profile` / `-p` is a repeatable,
comma-separated flag on `watch`, `sync`, `diff`, `render`, `images`, and `destroy`.
`*` activates every profile. `KSYNC_PROFILES` supplies comma-separated defaults.
Both sources trim whitespace and drop empty entries. An explicit flag replaces the
environment value entirely, including `-p ''`.

Explicit app names select exactly those apps in request order, regardless of
profiles. Dependencies outside that set are skipped, with a note on sync.
On the profile path, unknown profiles, an empty selection, or any `needs` dependency
outside the selection cause an error before cluster access. Errors list the
profiles that resolve the problem.

`Config.Select(names, profiles)` owns selection for all six commands. Command code
only resolves flag precedence. The engine and schedulers receive the selected apps.

# Consequences

Optional groups share dependency ordering with the core stack. Configs without
profiles retain their existing selection when no profile flag or environment
value is set. Positional names still support individual hook reruns and teardown.

A different selection never deletes apps it omits. `destroy` uses the same
selection rules and scope confirmation. `ksync destroy -p '*' --yes` deletes the
tracked resources of every app.

# Impact

A shell-wide `KSYNC_PROFILES` errors on configs that lack those profiles, unless
positional app names bypass profile selection.

# Alternatives

- **Separate config files:** lose cross-file dependency ordering and duplicate
  shared app definitions.
- **Replace positional names:** profiles only add to the unprofiled set, so they
  cannot select a subset of core apps.
- **Skip missing dependencies with a note:** retain this behavior for explicit
  names only. Profile selection rejects an inconsistent dependency set, as Compose
  does for dependencies excluded by profiles.
- **Ignore unknown profiles:** reject this permissive behavior to catch typos,
  consistent with unknown app names. This is a deliberate deviation from
  compose-go's `WithProfiles` behavior.
- **Root persistent flag:** keep the per-command convention of `-f` and
  `--context`. The supported order is `ksync sync -p debug`, not
  `ksync -p debug sync`.

# Notes

The selection model and name rule follow the Docker Compose
[profiles guide](https://docs.docker.com/compose/how-tos/profiles/) and
[profiles specification](https://docs.docker.com/reference/compose-file/profiles/).
Explicit ksync app names retain exact targeting rather than Compose's automatic
dependency inclusion.
