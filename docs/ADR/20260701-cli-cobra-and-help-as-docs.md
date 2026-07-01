---
date: "2026-07-01"
author: "motoki317"
status: "accepted"
---

# Context

ksync's help was thin and its user guide lived apart from the binary. The CLI was a hand-rolled
dispatcher over the stdlib `flag` package: a `run()` switch on `os.Args`, a per-command `flag.FlagSet`,
a custom `parseInterspersed` to permute flags and positionals, and `flag.PrintDefaults` for each
command's help — a bare flag list with no behavior, examples, or concept material. Everything a user
needed to understand ksync (the `ksync.yaml` schema, building from source, hook and sync-wave
ordering, the render/diff/apply strategy, prune safety, troubleshooting) lived in a 289-line
`docs/usage.md` maintained by hand, separate from the code it described.

Two manuals drift. The goal was to make the CLI help the self-contained guide — so a user learns
ksync from `ksync help` and `ksync <command> -h` without opening a doc — and then shrink the repo
docs to a pointer, leaving one source of truth.

# Decision

Adopt `spf13/cobra` for the command tree and move the guide into the CLI; keep the stdlib config
model. Reject `spf13/viper`.

- **cobra, not viper.** cobra (and its `pflag`) were already indirect dependencies via
  argo-cd/kubectl, so promoting cobra to direct is near-zero marginal cost, and it earns structured
  per-command help, command grouping, native interspersed flag/arg parsing (retiring
  `parseInterspersed`), and free shell completion. viper is a genuinely new dependency and would add
  a generic env/config/default precedence layer with no payoff against the typed, fail-closed
  `internal/config` (`yaml.UnmarshalStrict` + domain validation); it is not adopted.
- **The help is the guide.** Each command carries a `Long` description of its behavior and sharp
  edges plus an `Example` block, and the concept material becomes `ksync help <topic>` pages
  (`config`, `builds`, `strategy`, `hooks`, `troubleshooting`) — help-only commands whose `Run`
  prints the guide, so `ksync <topic>` and `ksync help <topic>` both work and they list under a
  "Concept guides" group. The prose lives in `help.go`, so the whole manual is reviewable in one
  place. `docs/usage.md` shrinks to a pointer into `ksync help`.
- **GNU `--long` flags; command bodies unchanged.** Flags standardize on pflag's `--long` form with
  `-f`/`-v` shorthands, ordered for reading (`SortFlags = false`, scope → behavior → render
  strategy). Each flag is pointer-bound in its command constructor, so a `runX` body reads `*flag`
  exactly as it did under stdlib `flag`: the framework swap changed parsing and help, not command
  logic. `--version`/`-V` and the `version` command both print the build version; `-V` (not cobra's
  default `-v`) keeps `-v` free for `--verbose`. `main` maps a `RunE` error to the exit code
  (`context.Canceled` → interrupt/130, else `ksync: <err>`/1) with cobra's own error and usage
  printing silenced, and each command keeps its per-invocation `signalContext` so the double-Ctrl-C
  force-quit is untouched.

This is a breaking flag-syntax change: single-dash long flags (`-timeout`, `-yes`, `-context`, …) are
no longer accepted — use `--timeout` etc. It is acceptable pre-1.0: ksync is not yet adopted
anywhere, and the sole external wrapper is updated in lockstep.

# Consequences

- A user learns ksync from the binary: `ksync help` is a one-screen overview (purpose, a minimal
  `ksync.yaml`, the safety invariant, the command and topic lists); `ksync <command> -h` gives
  behavior, flags, and examples; `ksync help <topic>` gives the concept guides. No second manual to
  open or keep in sync.
- Shell completion (bash/zsh/fish) comes free via cobra's `completion` command.
- Flags and positional app names interleave natively (`ksync sync web --force` and `ksync sync
  --force web` both work), so `parseInterspersed` is gone.
- `docs/usage.md` is a pointer, not a manual; README and AGENTS point at `ksync help` as the guide.

# Impact

- `cmd/ksync/cli.go` (new): `newRootCmd` and the per-command constructors, the shared pointer-binding
  flag registrars, the command/topic groups, and the `-V` version flag.
- `cmd/ksync/help.go` (new): all help prose — the root `Long`, each command's `Long`/`Example`, and
  the concept-topic pages and their `helpTopic` builder.
- `cmd/ksync/main.go`: the stdlib dispatch (`run`, `usage`, `summaryOf`, `newSubFlagSet`,
  `printSubUsage`, `loadConfig`, `parseInterspersed`, the flag-registrar helpers, the `subcommands`
  table) is removed; `main` now executes the cobra tree and maps errors. Each `runX` loses its
  flag-parsing preamble and takes pointer-bound flags plus the positional app names; the bodies are
  otherwise unchanged.
- `cmd/ksync/override.go`: `stringSlice` gains `Type()` for `pflag.Value` (the `--image IMAGE=REF`
  placeholder).
- `cmd/ksync/watch_overrides_test.go`: the two direct `runWatch` calls drive the real cobra tree via
  a new `executeKsync` helper, so the tests exercise actual flag parsing and dispatch.
- `docs/usage.md` shrinks to a pointer; `README.md` and `AGENTS.md` point at `ksync help`.
- `go.mod`: `spf13/cobra` promoted to a direct dependency; viper not added.

# Alternatives

- **Enrich the stdlib `flag` CLI in place** (add `Long`/`Example`/topic text to the existing command
  table, a custom flag formatter, help-topic printers). Lower risk — it keeps the tuned dispatch and
  avoids the flag break — and it delivers the same help content and doc minimization, which are the
  bulk of the value. Rejected once the flag reorganization and a breaking change were explicitly in
  scope: with those granted, cobra's clean `--long` surface, free completion, and idiomatic
  help-topic host outweigh the dispatch rewrite, and cobra was already a dependency. Absent that
  green light, enriching stdlib in place would have been the better risk/reward.
- **Keep `docs/usage.md` as a full manual alongside the CLI help.** Rejected: two manuals drift, which
  is the problem this change exists to remove.
- **Add viper for config/flag/env precedence.** Rejected: a real new dependency that duplicates and
  fights the typed, fail-closed `internal/config`; ksync's few env surfaces do not need it.

# Notes

Validated on the built binary: `ksync help` shows the overview with the commands and concept-guide
groups; `ksync sync -h` shows behavior, examples, and ordered flags with the `--image IMAGE=REF`
placeholder; `ksync help strategy` and `ksync config` print their guides; `-V`/`--version`/`version`
agree; an unknown flag or command exits 1 with a one-line `ksync:` error and no usage dump. `just
test` (incl. the leak guard, so no private name reached the help copy) and `just check` (gofmt, vet)
pass.

A flag-parse error appends `run 'ksync <command> --help' for usage` (via `SetFlagErrorFunc` on the
root, inherited by every subcommand) so a mistyped flag has a next step without reprinting the whole
usage block.

Known quirks, accepted:

- `-force`/`-file` (single-dash) are *not* rejected the way other single-dash long flags are, because
  `-f` is a value-taking shorthand: pflag binds any `-f…` token to `--file` and takes the rest as the
  path (`-force` → `--file=orce`, `-file=x` → `--file=ile=x`). Usually that path does not exist and the
  run fails at config load ("no config file at orce"), not at flag parse — so the flag-error hint above
  does not fire. In the rare case the swallowed path names a real config file, it is loaded silently,
  overriding even an explicit `-f`. Inherent to a value-taking shorthand; the supported forms are
  `--force` and, for the config path, `--file`/`-f`.
- Bare `ksync` (no command) prints the root help to stdout and exits 0, where the old stdlib dispatcher
  printed usage to stderr and exited 1. This matches kubectl/helm/docker and is harmless to the one
  wrapper (which always passes a subcommand); kept as cobra's default.
