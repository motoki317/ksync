---
date: "2026-06-30"
author: "motoki317"
status: "superseded"
---

> **Superseded by [20260701-non-terminal-log-streaming](20260701-non-terminal-log-streaming.md).**
> The heartbeat decided here read as noise on a real multi-app run — it reprinted a near-identical
> in-flight snapshot every 30s with only a climbing elapsed. The successor drops the heartbeat and
> the per-app aggregate line for the docker-buildx model: stream each build's command output live,
> prefixed by app/stage. The app-qualification problem this ADR identified, and the slowest-first
> timing recap it added, both carry forward.

# Context

ksync's live progress block (`internal/ui` `console`/`Pipeline`) is built for a terminal: per-app
groups animate in place, a spinner ticks, finished groups freeze as a tree. Off a terminal — piped, or
in CI such as GitHub Actions — that machinery is inert by design, and what remained was a thin
fallback that read badly:

- **No owning-app context.** A build/import stage printed its plain result line from `Stage.heading()`,
  which names the stage by its *label* (the image/batch name), not the app. Two apps that each build an
  image labelled `opa` both printed `✓ 🔨 Build opa 8.2s` — indistinguishable, so a reader could not
  tell which app built what. This was the reported complaint.
- **No liveness.** Between a stage starting and finishing — a multi-minute build, a health-gated deploy
  — nothing printed. A long run looked hung in a CI log.
- **A totals-only Summary.** The run Summary reported apps-synced and total duration, never *which* app
  or image took how long — the one breakdown a CI reader wants to find the slow stage.

The fix must not disturb the terminal UX, which works well; it changes only the non-terminal path.

# Decision

Give non-terminal output its own three-part shape — app-qualified completion lines, a periodic
heartbeat, and a slowest-first timing recap — gated entirely behind `!tty` so the terminal path is
byte-for-byte unchanged.

- **App-qualify every stage reference.** `Stage.heading()` now qualifies a build/import label with its
  owning app (`Build shop/ui`, not `Build ui`); the deploy stage still reads as `Deploy <app>`. This
  fixes the indistinguishable-duplicates complaint at its source, and the same heading leads the
  failure-output header in both modes.
- **Drop successful per-stage lines off a terminal; keep failures.** A succeeding build/import stage
  prints nothing as it finishes — the heartbeat and the per-app line carry it. A *failing* stage still
  prints its full captured output and an app-qualified `✗` line, so a failure is never silent.
- **One completion line per app, off a terminal.** When an app finishes, a build app commits a single
  line carrying each stage's label and time plus the group's total
  (`✓ shop  🔨 ui 1m30s · 📦 2s · 🚢 21 applied 3s  2m02s`); a deploy-only app keeps its existing
  `🚢 Deploy <app>` line. This replaces the dropped per-stage lines with one app-qualified line each.
- **A heartbeat coordinator (`internal/ui/heartbeat.go`).** `StartHeartbeat(w, c, interval, render)` is
  the inverse of `StartFooter`: active exactly when the animated block is **not** — gated on `isLive`,
  not strictly "off a terminal", so it also runs under `NO_COLOR` on a real terminal (which disables the
  block too) and fills the liveness gap there. It runs its own ticker (the console's animation ticker
  never runs while the block is empty, which off a live terminal it always is) and prints `render()`'s
  line every interval, or nothing when `render` returns empty. The default interval is 30s. The command
  layer supplies a closure that snapshots the in-flight stages and formats them —
  `⏳ 🔨 shop/ui 24s · 🚢 db 5s (3 not ready)` — so a reader sees the work advancing. The per-stage
  elapsed climbing each tick is the liveness signal; a waiting deploy also carries its health-gate tail
  (parenthesized so its own commas never read as item separators), so a stalled deploy names *what* it
  waits on rather than only an ever-climbing elapsed. There is no leading run-total to plumb.
- **A slowest-first timing recap.** Each app's completion line and total are captured at finish time
  into `progress` (the live pipeline map is empty by the end, so the recap cannot read it then); after
  the run's Summary, a `Timings` block prints those lines sorted by total descending. In watch mode the
  recap prints per batch from `onIdle` and then resets, like the per-batch Summary.

The heartbeat and recap are self-gating: `StartHeartbeat` is inert on a terminal, and `progress` only
captures a timing when the pipeline is non-terminal, so the recap is empty (and prints nothing) on a
terminal. The command layer wires both unconditionally and needs no `tty` branch of its own.

Lock discipline for the heartbeat: the snapshot copies the live pipelines under `progress.mu`, releases
it, then reads each stage under its own lock, releasing all UI locks **before** writing through
`liveTerm.line` — never holding a pipeline/stage lock across a console write. Off a terminal no pipeline
is registered with the console, so no `console.mu → pipeline.mu` path exists today; snapshotting first
keeps that true even if the predicate ever changes.

# Consequences

- The reported confusion is resolved: every build/import line, the heartbeat, and the recap name the
  owning app, so two apps building like-named images are never conflated.
- A long CI run shows progress every 30s instead of going silent, and ends with a `which app/image took
  how long` breakdown sorted to put the slow stage first.
- The terminal experience is untouched — the only shared edit (`heading()` gaining app context) affects
  the failure-output header, an improvement, and no success-path terminal output changes.
- Output volume stays bounded: per app, one completion line plus one recap line, and a heartbeat only
  while work is in flight (silent when idle), so the steady state of a watch loop prints nothing.

# Impact

- `internal/ui/pipeline.go`: `Stage.Done` drops the success line off a terminal (keeps the failure
  path); `heading()` app-qualifies build/import labels; `committed` routes an off-terminal build app to
  a new one-line form (`oneLine`); adds `Snapshot`/`RunningStage` (in-flight stages for the heartbeat)
  and `Recap` (the completion line + total for the timing recap).
- `internal/ui/heartbeat.go` (new): `Heartbeat`/`StartHeartbeat`/`HeartbeatLine`.
- `internal/ui/ui.go`: factors the `isLive` predicate shared by `startPipeline`, `StartFooter`, and
  `StartHeartbeat`.
- `cmd/ksync/main.go`: `progress` gains a snapshot accessor and a per-batch timing accumulator;
  `runSync` wires the heartbeat, prints the `Timings` recap, and cancels/drains eager builds before the
  Summary so a late-finishing eager build cannot print after it; the watch reporter wires the heartbeat
  and prints/resets the recap in `onIdle`.
- `internal/ui/*_test.go`: the non-terminal build test asserts the new one-line-at-commit behaviour
  (success path prints nothing before the commit); new tests cover `oneLine`, `Snapshot`, `Recap`,
  `HeartbeatLine`, and the silent-when-idle heartbeat.
- No change to loop/scheduler/engine semantics, nor to any terminal output.

# Alternatives

- **Keep per-stage lines, just add app context.** Rejected: it fixes the duplicates but leaves the
  liveness gap and doubles the volume against the heartbeat that already names in-flight work.
- **Host the heartbeat inside `console`.** Rejected: `console` is a low-level terminal/section
  coordinator; teaching it app/image semantics and a second off-terminal registry duplicates what
  `progress` already owns. The heartbeat lives beside `progress` and writes through the existing
  `ui.WriteLine` seam.
- **A leading run-elapsed on each heartbeat line.** Rejected: it needs batch-start plumbing that
  differs between one-shot and watch, while the per-stage elapseds already convey liveness.
- **Drop the per-app completion lines and rely on the recap alone.** Rejected: the completion lines give
  incremental "app done" feedback during the run; the recap is the sorted end-of-run digest. They serve
  different moments and are one line each.

# Notes

The heartbeat interval is a fixed 30s constant rather than a flag or env var — surface kept minimal per
the project's UX lens. Tests inject a short interval directly into `StartHeartbeat`, so the default
needs no override for testability. Builds on the live-block design (ADR 20260615-grouped-pipeline-
progress) and the section-spacing contract (ADR 20260619-section-output-spacing): the heartbeat and
recap are `SectionPipeline`/`SectionSummary` writes, so the console spaces them automatically.
