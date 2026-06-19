---
date: "2026-06-19"
author: "motoki317"
status: "accepted"
---

# Context

ksync's human output is a sequence of distinct sections — a startup log line, the Plan, the streamed
per-app pipelines, the run Summary, the watching-for-changes log — and a single blank line between
them is what keeps a run legible. That spacing was hand-rolled at each producer: `printPlan` baked in
a leading `\n` and a trailing `\n\n`, `summaryLines` a leading `""`, `onIdle` an extra `WriteLine("\n")`,
the picker its own leading `""`. Because every blank was the *previous* writer's responsibility, a new
adjacency had no separator unless someone remembered to add one — and the producers cannot see each
other, so they cannot coordinate. This produced a recurring class of bug, reported repeatedly: most
visibly, a rebuild's pipeline glued directly onto the `Finished, watching for changes` line, because
the pipeline carried no leading blank and nothing downstream of the log line inserted one.

Nearly all output already funnels through one process-wide coordinator (`internal/ui` `console`/
`liveTerm`), which serializes writes and paints the live block. It is the one place that sees every
section in order — the natural owner of inter-section spacing.

# Decision

Make blank-line separation a structural property of the `console`, not a per-caller convention.

- Every committed write is tagged with a `Section` kind: `SectionLog`, `SectionPlan`, `SectionSummary`,
  `SectionPipeline`. The console records the last section written; before a write whose kind differs
  from the last, it emits exactly one blank line. The zero value (`sectionNone`) means nothing has been
  written, so the top of output carries no leading blank; a `lastBlank` flag suppresses a separator when
  the previous write already ended blank, so blanks never double up.
- The live block (a pipeline group or the run-summary footer) is one `SectionPipeline` section: its
  first attach (`beginBlock`) emits the leading separator and marks the section, so the block's own
  item commits do not separate from one another. The within-block "items vs pinned footer" blank moved
  into `drawBlock`, independent of section separation — so the footer-only convergence start shows one
  blank, not two.
- Producers emit only their own content and a section kind. The hand-rolled blanks are gone:
  `printPlan` no longer brackets itself, `summaryLines` drops its leading `""`, `onIdle` drops its extra
  `WriteLine("\n")`.

The picker (`ConfirmBuilds`) keeps its own leading blank: it runs in raw mode and bypasses the console
entirely (it self-erases on dismissal, leaving console state untouched), so the console cannot space it.
One self-managed blank there is consistent with the rule.

# Consequences

- The reported bug is fixed structurally: a rebuild after `Finished` is a `SectionPipeline` block
  starting against a `SectionLog` line, so the console inserts the separating blank — verified by a
  terminal-model test that replays the console's actual erase/cursor-up/newline bytes and asserts the
  reconstructed scrollback (one blank between every section; the rebuild one blank below the watching
  line; no leading or doubled blanks).
- A new kind of output is separated automatically — the failure mode "someone forgot the blank" is no
  longer reachable for console-routed output, which is the robustness the change was asked to provide.
- Spacing is now identical on a terminal and off it (CI/pipes): the same `commit` path runs, only the
  erase/repaint is a no-op without a live block.

# Impact

- `internal/ui/console.go`: adds the `Section` type, `lastKind`/`lastBlank` state, `needSep`/
  `beginBlock`/`endsWithBlankLine` helpers; `line`/`finishItem`/`addItem`/`setFooter` thread the kind;
  `drawBlock` owns the items-vs-footer blank; `WriteLine` gains a `Section` argument.
- `internal/ui/log.go`, `internal/ui/pipeline.go`: tag their writes (`SectionLog`, `SectionPipeline`).
- `cmd/ksync/main.go`: `printPlan`/`printSummaryBlock`/`summaryLines` drop their hand-rolled blanks and
  pass a kind; `onIdle` drops the manual separator; the gate skip note and deploy-fallback line are
  tagged.
- `internal/ui/console_test.go`: `line()` calls gain a kind; the height-clamp expectation shifts (the
  footer's separator row now counts against the budget, so one fewer item fits — "… 5 more").
- `internal/ui/console_render_test.go` (new): the terminal-model regression test for section spacing.
- No change to loop/scheduler/engine semantics; output content is unchanged, only its spacing is now
  centrally owned.

# Alternatives

- **Keep per-caller blanks, just add the missing one.** Rejected: it fixes this instance, not the
  class — the next adjacency reintroduces the same bug, which is exactly the repeated-feedback pattern
  this replaces.
- **An explicit `console.NewSection()` boundary marker** callers invoke between sections. Less code, but
  it is the same "remember to call it" failure mode in a new shape; tagging each write and letting the
  console infer transitions cannot be forgotten.
- **Tag only at section starts (not every write).** Equivalent for well-behaved producers but loses the
  `lastBlank`/`lastKind` bookkeeping that makes interleaved log lines and the live block compose
  correctly; tagging every committed write keeps the state machine total.

# Notes

The terminal-model helper (`vterm`) interprets only the four control sequences the console emits (CR,
ONLCR newline, `\x1b[K`, `\x1b[1A`). It exists because a plain byte buffer cannot show the final
scrollback — the live block is painted then erased/redrawn in place — and the spacing contract is
precisely about what survives on screen. It is the durable guard for "exactly one blank between
sections," the requirement behind the repeated reports.
