---
date: "2026-06-16"
author: "motoki317"
status: "accepted"
---

# Context

`ksync watch` rebuilds and redeploys on every file change. A source edit dirties the build and the
deploy schedulers, both fire after the debounce, and docker build → image import → sync runs with no
human in the loop (`internal/loop`, the `watcher.Events` path marking the schedulers dirty). That is
the right default for a hands-off convergence, but for a tight inner-dev loop it is *too* eager: a
developer who saves mid-edit — a syntax-broken file, a not-yet-finished refactor, a throwaway
`println` — pays for a docker build and a cluster sync they did not ask for. With several images in
one context, one save can kick off several expensive builds at once.

The user wants the loop to **default to rebuilding only on an explicit choice**, and to choose *which*
images to rebuild — per-image, not all-or-nothing — through an interactive prompt rather than typed
commands. The build/deploy split already in place (two `Scheduler` instances; 20260616-eager-build-ahead)
means "which images" and "which deploys" are already separable units of work; the missing piece is a
gate between detecting a change and scheduling it.

A constraint shapes the design: ksync's live progress block (`internal/ui/console`) paints animated
lines to the terminal via a 100ms ticker, and a raw-mode interactive picker needs exclusive,
un-animated control of that same terminal. The two cannot run at once without the ticker clobbering
the user's keystrokes.

# Decision

Add an opt-out **manual gate** to the watch loop: on an interactive terminal, incremental changes are
held for an explicit build/deploy choice instead of being scheduled automatically. `-auto` (or a
non-interactive stdin, which has no one to answer) keeps the pre-existing automatic behavior.

**The gate intercepts only the incremental `watcher.Events` path.** Startup convergence is unchanged —
every app and build is still marked dirty up front and converges automatically, so the cluster matches
the working tree before the gate ever engages. When a gate is configured, a change accumulates into a
*pending set* (`pendingBuilds` per app/entry, `pendingDeploy` per app) rather than marking the
schedulers; once the loop is idle and the change burst has settled (the existing debounce), the loop
hands the pending list to the gate and acts only on what comes back.

**The selectable unit is per-image.** The prompt lists one 🔨 row per dirty image (labeled by the new
`apps[].build[].name`, defaulted from the image's last path segment, unique within an app) and one 🚢
row per app whose manifests changed with no image pending (a rebuilt app redeploys anyway, so that row
would be redundant). Choosing an image rebuilds it *and* redeploys its app; choosing a 🚢 row just
redeploys. Unchosen images stay pending and are re-offered once the loop is next idle, until Skipped.

**The picker is a two-step interactive component** (`internal/ui/prompt.go`), modeled on Claude Code's
AskUserQuestion CLI: a single-select menu — **Build all** (cursor default), **Select which to build**,
**Skip** — then, only if Select, an arrow-key, space-to-toggle multi-select. The decision logic is a
pure model (`buildPrompt`, unit-tested by feeding key events); a thin raw-mode driver (`ConfirmBuilds`)
translates terminal bytes to key events and repaints, entering raw mode only for the picker's lifetime.

**The picker runs only while the loop is idle** (no build/deploy in flight) — the invariant that tames
the raw-mode ↔ animated-block conflict. When idle, the live block is empty and its ticker stopped, so
the picker safely owns the terminal, then restores cooked mode before any build animates. Changes that
arrive while work runs accumulate silently; the prompt reappears once the loop returns to idle.

The loop stays terminal-free and testable: the gate is injected as `loop.Gate{Ask, Decisions}`. `Ask`
is a non-blocking call the loop makes when idle (the cmd layer launches one async picker goroutine);
the chosen items return on `Decisions`. A quit (Ctrl-C / `q` — the terminal sends no SIGINT in raw
mode) cancels the run via a context the cmd layer derives for exactly this.

# Consequences

- A mid-edit save costs nothing until the developer chooses; the common "just rebuild everything" is
  one Enter (Build all is the default), and a specific image is a few keystrokes.
- Per-image granularity means editing one service in a shared monorepo context need not rebuild its
  siblings — the developer picks the one that matters and leaves the rest pending.
- Every downstream invariant holds: a chosen item flows through the same schedulers, eager-build split,
  `needs` gating, health gate, and retry-backoff as before. The gate only decides *whether and when* an
  incremental change enters that machinery.
- **"Press Enter to resync all" is removed** — the picker owns stdin, and the change-driven prompt
  supersedes it. Re-saving any watched file re-opens the prompt; `ksync sync` remains the one-shot
  re-apply. The loop's `Resync` channel is kept (still unit-tested) but no longer wired to the keyboard.
- Non-interactive runs (CI, pipes, process managers) are unaffected: no TTY ⇒ no gate ⇒ auto-rebuild.

# Impact

- `internal/config`: `Build.Name` (defaulted from the image, unique within an app). Validated in
  `Parse`; `TestParse_BuildName*`.
- `internal/loop`: `PendingItem`/`Decision`/`Gate` types and `Options.Gate`; the pending-set state
  machine (`pendingBuilds`/`pendingDeploy`, `maybePrompt`, `release`, a `Decisions` select case, and a
  prompt deadline folded into the loop timer). A nil gate is the unchanged auto path. Tested with a
  fake gate (`TestRun_Gate*`): held-until-decision, skip, manifest-only item, and subset-leaves-remainder.
- `internal/ui`: `PromptItem`, the pure `buildPrompt` model and `decodeKeys` (both unit-tested), and the
  `ConfirmBuilds` raw-mode driver (reusing `Colors`, the stage icons, `displayWidth`, and the block's
  erase/redraw constants). On entry it flushes any input queued while the loop sat idle in cooked mode
  (`flushInput`), so a stray Enter typed before a change cannot confirm the default (Build all) the
  instant the prompt opens — without it the prompt appears to be skipped entirely.
- `cmd/ksync` (`runWatch`): the `-auto` flag; `interactiveTerminal` detection; a cancelable context so
  the picker can quit; `buildGate` wiring `loop.Gate` to one async `ConfirmBuilds` goroutine; the build
  stage label now uses `Build.Name`. `resyncOnEnter` (and its `bufio` use) removed.
- Risk: the picker relies on `os.Stdin.SetReadDeadline` to stay responsive to context cancellation
  during a blocked read. On the rare terminal where deadlines are unsupported the read simply blocks
  until a keypress (the keyboard still works; only SIGTERM-during-prompt is delayed), and `MakeRaw`
  failure falls back to Build all rather than dropping the pending changes.

# Alternatives

- **Typed-number selection** (`1 3`, Enter=all, `n`=skip) over a printed list. Rejected: the user asked
  for an arrow-key/spacebar picker; numbers are error-prone for a per-image list and re-printing the
  menu on every change clutters scrollback. The interactive picker repaints in place and erases on exit.
- **Keep auto-rebuild the default, add a `-manual` opt-in.** Rejected: the explicit request was for
  manual to be the default; `-auto` is the escape hatch.
- **Gate startup convergence too.** Rejected: startup must converge the cluster to the working tree
  before incremental editing begins — prompting before the first sync would leave the cluster
  arbitrarily stale and defeat "the cluster matches your files on start."
- **Prompt while work is in flight** (raw mode concurrent with the animated block). Rejected: the
  ticker repaint clobbers keystrokes. "Prompt only while idle" sidesteps it entirely, at the cost of
  deferring a prompt until the current build/deploy finishes — acceptable, since you cannot change a
  build already running.
- **A persistent idle-time stdin reader to keep on-demand resync** (e.g. press `r`). Rejected: it must
  hand control of stdin to the change-driven picker, reintroducing the raw-mode coordination the
  idle-only invariant avoids; re-saving a file or `ksync sync` covers the need.
