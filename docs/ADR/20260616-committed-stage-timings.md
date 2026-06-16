# Freeze the per-app stage tree on commit

Date: 2026-06-16
Status: accepted

## Context

The grouped per-app pipeline (ADR 20260615-grouped-pipeline-progress) renders a build app's live
group as a tree — 🔨 Build → 📦 Import → 🚢 Deploy, each row carrying its running elapsed. But on
commit the whole group was *replaced* by a single line, `🚢 Deploy <app>  N applied  <took>`, where
`took` was the app's end-to-end wall clock. Two problems surfaced during dogfooding a whole-stack
sync:

- **The per-stage build/import timings were lost.** Once the run finished, scrollback held only the
  deploy line. There was no way to see afterward how long each build took — exactly the number a
  developer tuning a slow build wants. For build apps `took` was also misleading: builds run ahead
  of the needs DAG (ADR 20260616-eager-build-ahead), so an app's end-to-end time mostly overlapped
  its dependencies' and did not reflect any single stage.
- **The committed line's "Deploy" word collided with the in-pipeline Deploy stage.** With the tree
  gone, every app's sole surviving line read `🚢 Deploy <app>` — the same kind word the live tree
  uses for one stage among several — so the line looked like the app only deployed, when it had also
  built and imported.

## Decision

Commit the **frozen stage tree** for a build app instead of collapsing it, and drop the redundant
kind word from the single-line form.

- A build app (`expand`) commits its app header plus one row per stage, each stamped with its final
  ✓/✗ and its own elapsed — so every build, import, and the deploy retain their time in scrollback.
  The deploy row additionally carries the apply summary and health symbol (which the pipeline does
  not itself know). Rows stay phase-ordered and column-aligned, the live tree simply frozen.
- A build-less app still commits one line, now `🚢 <app>  N applied  <elapsed>` — no "Deploy" word.
  The 🚢 icon and the "N applied" summary already say what happened and the app is the subject, so
  the line cannot be mistaken for the in-pipeline Deploy stage.
- Every committed deploy time is the **deploy stage's own** elapsed (apply + health gate), read from
  the pipeline, not the app's end-to-end `took`. Build and apply times are therefore reported
  separately and each is the stage it names.

The deploy outcome the pipeline cannot track — apply summary, health symbol, per-resource
failure/degraded lines — is passed in as a `ui.CommitInfo`; the pipeline owns the layout (alignment,
stage rows, the single-line `DeployLine`, shared with `ksync destroy`). Off a terminal nothing
changes: the build/import stages already stream their own result lines as they finish, so a pipe
commits just the single deploy line.

## Consequences

- Per-stage build/import/deploy timings survive the run; the slow stage is visible in scrollback.
- The committed output reads unambiguously — a build app shows its whole tree, an infra app one
  tight line — and the live → committed transition is continuous (the tree freezes in place).
- Layout lives in one place (`internal/ui`), so the committed deploy line and `destroy`'s line, and
  the live and committed trees, cannot drift apart.

## Impact

`internal/ui/pipeline.go` and the `cmd` summary path only; no engine/render/scheduler change. The
committed deploy time now excludes render (there is no Render stage), so it reads slightly shorter
than the old end-to-end `took`; the run's total still appears in the Summary footer. The live view
keeps its explicit kind words (the accessibility rationale of ADR 20260615 — the emoji is never the
only signal while you watch); the committed tree's build/import rows lean on the icon plus the image
label, the denser frozen form a scrollback summary can afford.

## Alternatives

- **One compact line per app with a `🔨 build-total · 🚢 deploy` breakdown.** Tightest (20 apps stay
  20 lines), but sums multiple build stages into one number, losing the per-build detail that is the
  point of inspecting afterward.
- **A "slowest stages" row in the Summary footer.** Compact, but surfaces only the top few and not
  every app's breakdown.
- **Keep the kind word in the committed tree (literally freeze the live rows).** Most consistent with
  the live view, but re-introduces the `Deploy` word the change set out to disambiguate; the icon
  plus the app header and build rows already make each row's kind clear.
