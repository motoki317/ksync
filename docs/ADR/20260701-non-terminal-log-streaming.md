---
date: "2026-07-01"
author: "motoki317"
status: "accepted"
---

# Context

The previous non-terminal design (ADR 20260630-non-tty-progress-output) gave a piped/CI run three
things: app-qualified completion lines, a 30s heartbeat, and a slowest-first timing recap. Run on a
real 22-app sync it read as a letdown. The heartbeat was the problem: between a multi-minute build's
start and finish it reprinted a near-identical in-flight snapshot every 30s, each line differing only
by a climbing elapsed (`⏳ 🔨 platform/rust 3m30s` … `7m30s`, eight times). That is noise, not progress —
the reader learns nothing from the reprint, and the genuine signal (what the build is actually doing)
never appears at all, because a succeeding stage streamed *nothing* off a terminal by design.

docker buildx is the reference for getting this right on both surfaces. On a terminal it animates a
summarized view; off a terminal it simply streams the build command's own log lines, each tagged with a
short step identifier. The log itself is the liveness signal — no synthetic periodic status is needed.

# Decision

Adopt the buildx model for the non-terminal path: stream each command's output live, prefixed by the
app and stage it belongs to, and remove the heartbeat. The terminal path is unchanged.

- **Stream build output, prefixed, with running elapsed.** Off a terminal, `Stage.Write` accumulates
  bytes and emits each complete line as one console write, prefixed `app/label <phase> [<elapsed>] │ ` —
  `shop/ui build [45s] │ Compiling foo`. The phase word (`build`/`import`/`deploy`) is greppable and
  disambiguates a build from an import that share a label; a label equal to the app name collapses
  (`web build`, not `web/web build`). The per-line elapsed is the stage's running time, mirroring
  buildx's `#12 45.3s text`, so a reader sees how long a build has run at a glance — without the
  heartbeat reprint this ADR removed. It sits **inside the prefix, left of the `│`**, so everything
  left of the bar is metadata and everything right is the command's own output — a bare `45s` right of
  the bar would read as a line of build output. The result line follows the same shape
  (`shop/ui build [1m01s] │ ✓`), so the bracket is always the same column. Concurrent apps' lines
  interleave, exactly as buildx does — grouping by app would hide liveness again.
- **No heartbeat.** The 30s ticker, its snapshot plumbing (`RunningStage`, `Pipeline.Snapshot`,
  `progress.snapshot`), and `internal/ui/heartbeat.go` are removed. The streamed log lines *are* the
  liveness signal, so there is nothing to reprint.
- **Imports do not stream; they report a result.** `build.Loader` coalesces a load and tees one
  command's output to every waiting app's stage (`multiOut`), so streaming per stage would print each
  import line once per app. Instead an import stage buffers its output (off a terminal) and prints only
  a result line on success; on failure it dumps the buffer so the break stays diagnosable. Imports are
  short (the build is the long pole), so nothing material is lost.
- **Deploys stream on change, not on a timer.** A deploy has no command output; its progress is the
  health gate. `Stage.SetTail` (called each poll) streams a line only when the not-ready status
  *changes* — `db deploy [12s] │ waiting for health: 1 not ready (…)` (same bracketed running-elapsed
  prefix as a build line) — so a deploy stuck on the same resource
  stays silent, exactly as buildx does for a step that produces no output. The engine reports the
  not-ready set (`SyncOptions.OnWait`) from **both** wait phases: the sync operation loop (a
  health-gated wave or a non-hook Job) and the final post-apply health gate. The operation-loop call is
  what makes this fire for a waved or hooked app at all — that app's final gate is already healthy by
  the time it runs (the operation loop gated on health), so without it a long wave or migration would
  be silent off a terminal. An empty set is skipped (hooks are excluded from the not-ready computation,
  so a lone PostSync Job would otherwise report a misleading "0 not ready"). The deploy's result is its
  committed line (`✓ 🚢 Deploy <app>  N applied  Ns`), printed at `Pipeline.Finish`.
- **Per-stage result line on finish.** A build/import stage prints `app/label <phase> │ ✓ <elapsed>`
  (or `✗`) when it finishes. A build that already streamed every line keeps no full buffer and is not
  re-dumped on failure (that would double the output); only imports, which never streamed, are dumped.
- **Keep the slowest-first timing recap.** The run's original requirement was two parts — progress
  *during* the run and a `which app took how long` summary *at the end*. Only the first was at fault, so
  the recap (`Pipeline.Recap` → the `Timings` block after the Summary) carries forward unchanged. The
  per-app *inline aggregate* line is dropped: once each stage streams its own result, an app's
  end-to-end record is the recap, not a second inline line.

# Consequences

- A long CI build now shows its actual progress — the buildx log, line by line — instead of a silent
  gap punctuated by a reprinted elapsed. The reader sees what the build is doing, not just that time is
  passing.
- Output volume tracks the build's own verbosity (as buildx does), rather than a fixed per-interval
  line. A deploy-only app stays as terse as before: one `🚢 Deploy` line, plus a health-wait line only
  while it genuinely waits.
- The terminal experience is untouched: `Stage.Write`/`SetTail`/`Done` branch on `!tty`, and the
  on-terminal buffer-and-tail behaviour, the frozen stage tree, and the failure expansion are all
  unchanged.
- The end-of-run `Timings` recap still answers "which app/image took how long," sorted slowest-first.
- One wait stays silent off a terminal: a lone long-running PostSync hook with no other pending
  resource. Hooks are deliberately excluded from the not-ready set (so a running hook does not read as
  a stuck deploy), and ksync does not stream an in-cluster Job's logs, so the hook's progress is not
  surfaced live — its outcome appears in the committed `🚢 Deploy` line, or, if it wedges, in the
  timeout diagnostics. Streaming a "waiting for hook X" line would need an operation-specific pending
  helper that reports present-but-unhealthy hook resources; deferred until that wait proves a felt gap.

# Impact

- `internal/ui/pipeline.go`: `Stage` gains a `partial` line buffer; `Write` streams off a terminal
  (`writeOff`/`takeLines`/`emitLine`/`streamPrefix`) for build stages and buffers import output; each
  streamed content line (`emitLine`, `SetTail`) is prefixed with the stage's running `Elapsed` (read
  under `s.mu`); `SetTail` streams a changed deploy status off a terminal; `Done` (`doneOff`) flushes the partial line
  and prints a per-stage result, dumping the buffer only for a failed import; `committed` routes an
  off-terminal build app to its deploy line (the stages already streamed); `Snapshot`/`RunningStage`
  and `plainResult` are removed. `Recap`/`oneLine` stay (recap-only now).
- `internal/ui/format.go`: adds `streamClean` (strips ANSI and collapses a `\r` redraw to its final
  frame, preserving inner spacing — unlike `sanitizeLine`, which collapses it for the single live row)
  and `ansiOnly` (an ANSI regex that keeps `\r`, so the redraw survives stripping to be interpreted).
- `internal/engine/sync.go`: the sync operation loop now calls `OnWait` with the non-empty not-ready
  set each poll, so a deploy streams progress during waves/hooks, not only in the final health gate.
- `internal/ui/heartbeat.go` and `heartbeat_test.go`: removed.
- `cmd/ksync/main.go`: drops `heartbeatInterval`, the heartbeat wiring in `runSync` and the watch
  reporter, and `progress.snapshot`. The `Timings` recap wiring is unchanged.
- `internal/ui/streaming_test.go` (new): off-terminal streaming, partial-line flush, build-failure
  (no re-dump), import (no stream, dump on failure), and deploy stream-on-change-only.
- No change to loop/scheduler/engine semantics, nor to any terminal output.

# Alternatives

- **Keep the heartbeat, just lower its frequency / dedupe reprints.** Rejected: the heartbeat reprints a
  *summary* of in-flight work, never the work's own output, so it can never show what a build is doing.
  Streaming the real log subsumes it.
- **Stream import output too, deduping the coalesced tee.** Rejected for now: it needs the UI-agnostic
  `Loader` to learn tty-ness and a combined prefix, or a shared stream writer threaded through the
  command layer. Imports are short, so buffering-and-reporting is the simpler correct choice; revisit if
  import logs ever become a diagnostic need.
- **Group each app's streamed lines together.** Rejected: buffering until an app finishes to print its
  block contiguously reintroduces the silent gap. Interleaving is what makes concurrent work look live.
- **Drop the timing recap too, for maximum simplicity.** Rejected: the recap was never the complaint,
  and the original ask explicitly wanted an end-of-run "which app took how long" summary.

# Notes

Validated on a real non-terminal run (docker-desktop, two concurrent docker builds plus a deploy-only
app, stderr redirected to a file): the two builds' buildx logs interleaved under `app build │` prefixes,
the deploy-only app printed its `🚢 Deploy` line, and the run ended with the `Timings` recap — no
heartbeat reprints. Builds on the live-block design (ADR 20260615-grouped-pipeline-progress) and the
section-spacing contract (ADR 20260619-section-output-spacing): every streamed line is a
`SectionPipeline` write, so the console spaces sections automatically.
