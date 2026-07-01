# Group total on the header, and the deploy-only line

Date: 2026-06-18
Status: accepted

## Context

ADR 20260615-grouped-pipeline-progress grouped each app's progress into a 🔨 Build → 📦 Import → 🚢
Deploy tree; ADR 20260616-committed-stage-timings froze that tree on commit and, for an app with no
tree, committed a single `🚢 <app>  N applied  <took>` line that deliberately **dropped** the
"Deploy" word. The reasoning then: the single line could surface for an app that had in fact built
(the collapse keyed on whether a tree had rendered), so the word would have implied "only deployed"
when the app had also built and imported.

Three gaps remained, surfaced dogfooding a whole-stack sync:

- **The tree header showed only the app name.** There was no single number for "how long did
  `depot` take" — only the per-stage rows. Comparing apps meant eyeballing the slowest row.
- **An override-only app still committed as a one-child tree.** An app whose images were all supplied
  as overrides (ADR 20260616-image-override) builds nothing, so its pipeline holds only the deploy
  stage — yet because the app *has* build entries in config (`expand`), it committed as a header over
  a lone `🚢 deploy` row. That is noise for what is really a single deploy.
- **The single line lacked the word the tree's deploy row carries.** The in-tree deploy row reads
  `deploy`; the single line named only the app. Once override-only apps also collapse to the single
  line, that gap reads as an inconsistency rather than a disambiguation.

## Decision

1. **The header carries the group's total wall-clock** — the span from the earliest stage start to
   the latest stage end (or to now, while running), shown on both the live and committed tree header
   in the same threshold color as every other time. It is the span, **not the sum** of the rows:
   builds overlap, so the sum would over-count. A pending-only group shows no total (nothing has
   started to time).

2. **Any deploy-only pipeline collapses to the single line, regardless of `expand`.** The committed
   view keys on whether build/import rows actually exist (`hasBuildStages`), not on whether the app
   *could* build. The live view collapses a deploy-only build app once its deploy **starts** (it then
   has no rows still to come), while keeping the tree open with a pending deploy row beforehand — so a
   normal build app's upcoming work stays visible until its builds begin.

3. **The "Deploy" word returns to the single line.** Because (2) makes the single line appear *only*
   for an app that genuinely did not build, the word is now accurate: it matches the in-tree deploy
   row instead of masking build+import work. `ui.DeployLine` takes a `kind` argument — sync passes
   "Deploy"; `ksync destroy` passes "" (it is not a deploy, and its icon and summary already say so),
   so destroy's line is unchanged.

This **amends bullet 2 of ADR 20260616-committed-stage-timings** ("a build-less app … no 'Deploy'
word"). The word is restored on the narrower, now-correct grounds that the single line means "only
deployed" — the very ambiguity that justified dropping it no longer exists once (2) holds.

## Consequences

- Each app's total is visible at a glance beside its name, while the per-stage rows keep the
  breakdown — the slow app and its slow stage are both findable in scrollback.
- An override-only app reads as one tight line like any other deploy-only app, not a one-child tree.
- The single line and the in-tree deploy row name the stage the same way, and the live → committed
  transition stays continuous.

## Impact

`internal/ui/pipeline.go` and the two `cmd` `DeployLine` call sites only; no engine/render/scheduler
change. `DeployLine` gains a `kind` parameter (destroy passes ""). The total is a span, so for an app
whose stages do not overlap it equals the sum, but for overlapping builds it reads shorter than the
sum of the rows — intentionally, since it is wall-clock.

## Alternatives

- **Sum the stage times for the header total.** Simpler, but double-counts overlapping builds — three
  parallel 30s builds would read 90s for ~30s of wall clock.
- **Keep override-only apps as a one-child tree.** Consistent with "has builds ⇒ tree", but a lone
  row under a header is pure overhead for a single deploy.
- **Leave the single line word-less (keep ADR 20260616 as-is).** Now that the line only ever means
  "only deployed", omitting the word loses parity with the tree's deploy row for no gain.
