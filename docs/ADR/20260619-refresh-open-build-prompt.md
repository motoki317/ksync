---
date: "2026-06-19"
author: "motoki317"
status: "accepted"
---

# Context

The manual build gate (20260616-manual-build-gate) holds incremental changes and, once the loop is
idle and the change burst has settled, asks the user which images/apps to rebuild via the picker
(20260619-single-view-build-picker). The picker is handed a snapshot of the pending items at `Ask`
time and runs in its own goroutine while the loop continues.

A change made *while a prompt is already open* was silently held: `maybePrompt` returns early when a
prompt is outstanding, so the new item only accumulated in the pending set and the open picker was
never told. The developer saw a prompt listing the first app, edited a second app, and nothing
reacted — the second app appeared only after the first was rebuilt and the loop returned to idle.
The desired behavior is to *build up* the pending set: a prompt left open should reflect every app
edited since, so one confirmation rebuilds them all.

This is constrained by how the picker reads input. No tty supports `os.File` read deadlines (verified
on macOS and Linux; see 20260619-single-view-build-picker), so the picker's read blocked until a
keystroke — it could not react to anything *between* keystrokes, including an external "refresh"
signal. So any fix has to change how the picker waits, not just notify it.

# Decision

**Abort-and-re-ask, driven by a non-blocking poll loop.** When fresh changes land while a prompt is
open, the loop aborts the stale prompt and re-asks with the full pending set; the picker re-shows
listing every pending app.

- **Picker waits by polling, not blocking.** `ConfirmBuilds` puts the terminal fd in non-blocking
  mode (`pollReader`, the unix non-blocking-read primitive already used by the freeze-fix `drainTTY`)
  and reads on a short tick (`pollInterval`), checking `ctx` and a per-prompt `abort` channel between
  reads. This is what lets an open prompt react without a keystroke — and, as a bonus, closes the
  latent gap where SIGTERM during an idle prompt was deferred until a key was pressed. Off unix it
  falls back to a blocking read (abort/ctx honored only on a keystroke). The fd is restored to
  blocking on every exit path — a leaked non-blocking stdin breaks the parent shell.
- **Abort is a distinct outcome, not a quit.** The picker resolves to exactly one of: a selection, a
  quit (Ctrl-C, cancels the run), or *aborted* (the loop asked it to refresh). An aborted picker acts
  on nothing; the gate reports `Decision{Reask: true}`. The loop clears its `prompting` flag only when
  it *receives* the outcome — never when it sends the abort — so a confirm racing an abort yields one
  clean outcome.
- **The loop owns when to abort.** A change arriving while `prompting` sets `abortPending`; once the
  debounce settles the loop calls `Gate.Abort` (a no-op if no prompt is open) and waits for the
  Reask. On Reask it re-asks immediately with a fresh `pending`/items snapshot. The prompt-deadline
  wake timer was extended to fire while a prompt is open with changes to fold in, or the abort would
  never trigger.

# Consequences

- Editing more files while the prompt is open folds them in: the picker re-shows with every pending
  app, and one confirmation rebuilds them all — the reported behavior.
- The picker now honors ctx cancellation (SIGTERM) within a poll tick on a real terminal, not only on
  the next keystroke.
- Each `Ask` remains a clean, independently-keyed `pending`/items pair, so the picker's returned
  indices always map back to the right `PendingItem`. This is why abort-and-re-ask was chosen over
  live in-place item refresh (see Alternatives).
- The refresh is a re-show: the cursor returns to "Rebuild all" and any prior narrowing is dropped.
  Acceptable — re-arming the safe default each time the set changes is defensible, and the dominant
  case (an untouched, all-selected prompt) looks seamless.

# Impact

- `internal/ui/prompt.go`: `ConfirmBuilds` gains an `abort <-chan struct{}` parameter and an `aborted`
  return; the read loop is split into `pollLoop` (non-blocking poll) and `blockingLoop` (off-unix
  fallback). `pollInterval` added.
- `internal/ui/tty_unix.go` / `tty_other.go`: `pollReader` (non-blocking read primitive) added beside
  `drainTTY`; the non-unix stub returns an error so the blocking fallback is used.
- `internal/loop/loop.go`: `Decision` gains `Reask`; `Gate` gains `Abort`; the loop tracks
  `abortPending`, aborts a stale prompt once the burst settles, handles `Reask`, and wakes at the
  prompt deadline while prompting.
- `cmd/ksync/main.go`: `buildGate` holds a per-prompt abort channel, wires `Gate.Abort` to close it,
  and maps an aborted picker to `Decision{Reask: true}`.
- Tests: `TestRun_GateFoldsNewChangeIntoOpenPrompt` (loop-level regression — a change during a prompt
  aborts and re-asks with both apps); `TestPollLoop_*` and `TestPollReader_*` (the abort/ctx/keystroke
  paths and the non-blocking read, all without a real tty).

# Alternatives

- **Live in-place item refresh** (push the new items into the open picker and repaint, preserving the
  cursor/selection). Rejected: the gate maps the picker's returned `selected []int` back into the
  `pending` slice captured at `Ask` time, so mutating the item set under a confirming picker desyncs
  that mapping and releases the wrong apps. Closing that needs mutex-coordinated state or selection
  keyed by stable identity — real complexity whose only payoff over re-ask is preserving a narrowing
  the user made *before* editing again, a rare case. Re-keying selection by item identity is the clean
  path to this feel later, if wanted.
- **A per-prompt reader goroutine** feeding a channel the picker selects on. Rejected: each prompt
  would leave a goroutine blocked on stdin, and across a watch session those readers accumulate and
  race the kernel for the next keystroke — lost keys. The single-goroutine non-blocking poll avoids
  any extra reader.
- **Notify the picker without changing how it waits.** Impossible on a tty: the blocked read cannot be
  interrupted, so a notification is not seen until a keystroke — exactly the bug.

# Notes

`pollReader` toggling `O_NONBLOCK` on the stdin fd is safe for the same reason `drainTTY` is: it runs
only after `MakeRaw` has succeeded — a real terminal, an fd the runtime poller does not manage — so it
cannot disturb `os.File`'s own non-blocking state.

`pollReader` folds an empty read (`EAGAIN`) and `n==0` (EOF) both to "no data, poll again," exactly as
`drainTTY` treats `n<=0` — deliberately *not* relying on `EAGAIN` being the only empty signal. The
freeze bug was precisely a tty where `os.File` semantics diverged from a pipe's, so the picker must not
vanish if some terminal reports 0 bytes for an empty non-blocking read. A genuinely closed stdin ends
the prompt through ctx (SIGHUP/SIGTERM) instead.

The risky logic is tested without a pty by calling `pollLoop` with an injected read and `pollReader`
over an `os.Pipe`, so no new module dependency (`creack/pty` is only an indirect one) and no
`vendorHash` churn.
