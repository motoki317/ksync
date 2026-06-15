# Grouped per-app pipeline progress display

Date: 2026-06-15
Status: accepted

## Context

ksync's live terminal output collapsed each external command (a docker build, an image import)
into one animated line and printed each app's apply as a separate one-line summary. During
dogfooding the stages read ambiguously: every line led with the same `✓` and an emoji whose
meaning was implicit, and in a whole-stack sync the build, import, and apply lines of different
apps interleaved in completion order, so it was hard to see which work belonged to which app or
what was still pending. The feedback was concrete: name the stage kind explicitly, group the lines
by app, and show upcoming stages (the deploy) while earlier ones run, so the remaining work is
visible while waiting.

The output layer was built around ephemeral, flat `track`s: an `Activity` created a track when a
command started and removed it when it finished. That model had no notion of an app, of stage
order, or of a stage that exists but has not started — none of which can be bolted onto a flat
list.

## Decision

Replace the flat tracks with a per-app **pipeline** model rendered as a grouped live block.

- A `Pipeline` is one app's progress group: an app header with its stages as indented rows. A
  `Stage` is one unit of work (a build batch, an import, the deploy) with a lifecycle
  pending → running → done/failed, and is the `io.Writer` a build/import command's output is wired
  to (tailing the latest line, buffering the rest, surfacing the full log only on failure).
- Stages are **named** — 🔨 Build, 📦 Import, 🚢 Deploy — and **phase-ordered** (build → import →
  deploy) regardless of when each is added. The deploy stage is created **pending up front**, so it
  shows as a dim `○` row beneath the builds: the upcoming work is visible while the builds run.
- An app with **no builds** (just a deploy) collapses to a single line, keeping infra apps compact;
  an app with builds always shows the full tree.
- The console is generalized from a flat `[]*track` to a `[]blockItem` (each renders ≥1 line), with
  the pipeline as the sole implementation and the Summary footer pinned below. The block is
  **height-clamped** to the terminal: when concurrent groups would overflow, item lines are elided
  with a `… N more` marker while the footer is always kept, so the cursor-up erase math can never
  break.
- On success an app's group **commits** a one-line summary to the scrollback and is removed from the
  live block; on failure (build, render, or sync) it is torn down so a watch retry starts clean. A
  new `loop.Options.OnError` hook gives the command layer the failure signal the loop previously only
  surfaced via a log line. The committed line carries the same explicit **🚢 Deploy** title as the
  live row (not an emoji alone), and its app name is padded to the run's widest so the `applied`
  column — and, when the counts read alike, the trailing duration — line up across the streamed
  per-app lines.
- Off a terminal the block is inert: build/import stages print a plain finish line, the deploy is
  silent (its committed summary is its record), so a pipe shows one line per app, not two.

## Consequences

- The three pieces of feedback are addressed directly: stages are named, work is grouped by app,
  and the pending deploy row shows remaining work while builds run. Verified live on k3d (TTY and
  pipe) and by unit tests asserting the rendered rows, ordering, collapse, lifecycle, and the
  non-terminal line sequence.
- One persistent per-app model now spans build → import → deploy, so the health-gate wait is just
  the deploy row's tail rather than a separate ad-hoc spinner — one fewer output primitive.
- The block can no longer overflow the screen and corrupt itself under high `--max-parallel`, which
  the old flat list left as a latent risk once groups (rather than single lines) are shown.

## Impact

- `internal/ui` churns: `Activity` and `Waiting` (and the `track` type) are removed in favor of
  `Pipeline`/`Stage`; shared duration/format helpers move to `format.go`; the console operates on
  `blockItem`s and gains width-and-height clamping. The command layer gains a small `progress`
  coordinator keyed by app name (safe because an app never runs concurrently with itself).
- `loop.Options` gains the optional `OnError` hook; existing callers are unaffected (nil = no-op).
- Behavior change for pipes/CI: the deploy no longer prints its own line (the per-app summary
  already reports it), so piped output is one line per app instead of two.

## Alternatives

- **One line per app, stages inline** (`🔨 Build 2/3 · 📦 Import · 🚢 Deploy`). Most compact and
  overflow-proof, but loses the per-build-entry detail the feedback explicitly asked to keep
  ("group by app *and its builds*").
- **Minimal: name + app prefix on the flat list.** Smallest change, but does not group by app and
  cannot show a stage that has not started — it misses the "remaining work" half of the request.
- **Keep `Activity`, add grouping on top.** Leaves two overlapping progress primitives and dead
  exported API; the pipeline subsumes Activity's job (tail + log-on-failure), so removing it is
  cleaner than layering.
