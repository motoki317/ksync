---
date: "2026-06-19"
author: "motoki317"
status: "accepted"
---

# Context

The manual build gate (20260616-manual-build-gate) prompts before rebuilding an incremental change.
Its picker was a **two-step** component: a single-select menu — Build all · Select which to build ·
Skip — and then, only on Select, a second screen with an arrow-key/spacebar multi-select that started
with nothing checked. Building one specific image cost a mode switch plus a fresh round of toggling:
Down, Enter (to reach the multi-select), then navigate and Space each wanted row, then Enter. The
extra screen and the "start from empty" multi-select were friction on the exact inner-loop action the
gate is meant to make cheap.

This supersedes the **picker UI** described in 20260616-manual-build-gate; the gate's loop semantics
(pending set, idle-only prompt, `loop.Gate`/`Decision`, eager-build split, skip-keeps-pending) are
unchanged.

# Decision

Collapse the two screens into **one master-detail view** with a single selection model:

```
3 changes pending — rebuild & deploy?

❯ ◉ Rebuild all
    ◉ 🔨 web     api-b
    ◉ 🔨 worker  api-b
    ◉ 🚢 shop    manifests only

↑/↓ move · Space toggle · Enter confirm
```

- A **Rebuild all** master checkbox sits above one row per change. The cursor defaults to the master
  with it **on**, so the common "rebuild everything" is a single Enter.
- Selection is a **mode, not a tri-state** (`all bool` + an explicit `checked []bool`). While `all` is
  on, items render dim/implied. Space on an item turns the master **off and selects only that item**;
  further Space toggles are additive. This is what makes "I want just this one image" a direct
  Down, Space, Enter (3 keys) instead of a clear-then-pick dance — and it is precisely why a classic
  tri-state parent checkbox is wrong here (tri-state would read Space-on-item as "all *except* this").
- **Enter always confirms the current selection** — no cursor-special-casing. The selection is every
  item when the master is on, otherwise the checked set; an empty set means **skip**.
- The **Skip menu row is removed**; the capability is not. Skip is "clear the selection, then Enter" —
  Space on the default master row (turning `all` off to an empty set), then Enter: a 2-key skip from
  the default cursor, with no reliance on Esc. Esc is deliberately *not* bound: it is also the first
  byte of every arrow-key escape sequence, so binding it to skip would misfire on a split read for the
  one action the gate exists to make easy.

`ConfirmBuilds`'s signature is unchanged (`selected []int, build, quit bool`), so the `cmd/ksync` gate
wiring and the `MakeRaw`-failure fallback to all-indices are untouched.

# Consequences

- "Rebuild everything" stays one Enter; "rebuild one image" drops from a mode switch plus re-toggling
  to Down, Space, Enter; skip is Space, Enter from the default cursor. Every path is arrow/Space/Enter
  and ≤3 keystrokes.
- One screen means no `step` state and no second render path — the model is a flat cursor over
  `len(items)+1` rows with one `handle`/`render`.
- The only deviation from a naive reading of "go back to Rebuild all and Enter" is that re-selecting
  all from an explicit subset is Up, **Space**, Enter (re-check the master) rather than Up, Enter. The
  alternative (Enter-on-the-master-row = all, regardless of its checkbox) was rejected because it
  leaves skip with no clean key and forces it onto the fragile Esc.

# Impact

- `internal/ui/prompt.go`: `buildPrompt` loses `step`/`topCursor` and the `topMenu` constants, gains
  `all` and `toggle`/`setAll`/`box`/`prefix`; `handle` is now a single switch and `render` a single
  view. `ConfirmBuilds`'s doc comment updated; the driver, `decodeKeys`, `flushInput`, and `erasePrev`
  are unchanged.
- `internal/ui/prompt_test.go`: the two-step key traces are rewritten as the new spec —
  `_SkipBuildsNothing` (`keySpace,keyEnter`), `_SelectSubset` (`keyDown,keySpace,…`), `_ToggleAll`
  (`a` toggles the master from the default), `_EmptySelectionIsSkip` (pick then unpick), `_Render`
  (asserts `❯ ◉ Rebuild all` and the per-item rows). `_BuildAllIsTheDefault` and `_Quit` are unchanged.
- `AGENTS.md`: the prompt.go blurb now describes the single-view picker.
- No change to `internal/loop`, `cmd/ksync` wiring, or any gate semantics.

# Alternatives

- **Keep the two-step picker.** Rejected: the second screen and empty-start multi-select are the
  friction this change removes.
- **Classic tri-state parent checkbox** (master derived from the children, Space-on-item = toggle that
  child within "all"). Rejected: it cannot express "pick only this one" from the all-selected state,
  which is the primary inner-loop action; the explicit `all` mode flag can.
- **Enter on the master row always means all** (cursor-special-cased). Matches the literal "go back to
  Rebuild all and Enter" in 2 keys, but leaves skip with no checkbox path and pushes it onto Esc, which
  can misfire against arrow-key escape sequences. Rejected for the clean, robust skip.
- **Bind Esc to skip.** Rejected for the same Esc/arrow-prefix ambiguity; the uncheck-all-then-Enter
  path needs no new key and cannot misfire.

# Notes

Shipped alongside this picker is a fix for a freeze that made the gate unusable on macOS, where it
was first exercised on a real terminal. `ConfirmBuilds` calls `flushInput` (drain stray cooked-mode
input) right after entering raw mode, and `flushInput` looped on `os.File.Read` under a short
`SetReadDeadline`. On a **macOS tty `SetReadDeadline` returns "file type does not support deadline"**
(verified: a tty is a character device the runtime poller does not manage), so the read had no
deadline, blocked until a keypress, and the loop never exited — swallowing every keystroke including
Ctrl-C (raw mode delivers no SIGINT) and never reaching the first `paint()`. The result: the first
post-startup edit hung with no prompt and an unresponsive terminal.

The fix: `flushInput` probes `SetReadDeadline` once; on failure it falls back to `drainTTY`
(`tty_unix.go`), a non-blocking `syscall` drain (`SetNonblock(fd, true)` → read until `EAGAIN` →
restore blocking) that returns immediately on an empty queue. Because it runs only after a deadline
failure — i.e. an fd the poller is not managing — toggling `O_NONBLOCK` directly cannot corrupt
`os.File`'s own state. A non-unix build gets a no-op `drainTTY` (`tty_other.go`). Verified end-to-end
by driving the real `ConfirmBuilds` inside a pty: the prompt paints, build-all / subset / skip / quit
all return correctly, and the no-input case no longer hangs before painting.

This **corrects the Risk note in 20260616-manual-build-gate**, which assumed a deadline-less terminal
would "simply block until a keypress (the keyboard still works)". That held for the main read loop but
not for `flushInput`, whose exit condition required the deadline; there the missing deadline was a hard
freeze, not graceful degradation. The main loop's residual behavior (a blocked read wakes on the next
keystroke, so SIGTERM during an idle prompt is deferred until a key is pressed) is unchanged and
genuinely benign — Ctrl-C is itself a keystroke. Quit is Ctrl-C only; `q` is not bound (it is a
plausible future filter keystroke, and Ctrl-C already aborts).
