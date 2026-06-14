---
date: "2026-06-14"
author: "motoki317"
status: "accepted"
---

# Context

ksync applies **server-side** (SSA, field manager `ksync`) but computes its diff
**client-side**: `engine.Sync` calls `diff.DiffArray(target, live, diff.WithLogr(...))`
with no structured-merge or server-side-diff option. The diff feeds two decisions
each reconcile — whether the app is `Modified` (which gates `skipHooks`) and, via
`WithResourceModificationChecker`, which objects are applied.

Driving a whole multi-namespace stack (~16 apps, dogfooding) surfaced the mismatch
this creates. Every namespace converged to `0 applied` on a warm re-sync **except one
StatefulSet-based service**, which perpetually reported `1 applied`. The drifting
object is its StatefulSet: the chart's render omits
`spec.volumeClaimTemplates[].volumeMode`, the API server defaults it to `Filesystem`,
and the client-side diff — which compares ksync's desired against the server-defaulted
live object without accounting for SSA field ownership — sees that server-added field
as drift on every sync. VolumeClaimTemplates are immutable, so the apply is a harmless
no-op, yet the diff never goes quiet.

This is a well-known Kubernetes SSA-vs-client-diff gotcha, not unique to ksync:
ArgoCD shows the same StatefulSet as perpetually `OutOfSync` under its legacy
(client-side) diff, and added Server-Side Diff specifically to fix it.

# Decision

Keep the **client-side diff** for now and accept the spurious StatefulSet-VCT drift
as a known limitation. Do **not** rush a diff-engine change into the whole-stack
hardening pass. Record the fix path (below) for a separate, properly-validated change.

# Consequences

- Behavior stays ArgoCD-default-consistent and the sync path is unchanged — no risk
  introduced during the whole-stack validation.
- A StatefulSet whose chart omits a server-defaulted VCT field reports `1 applied`
  on every sync instead of `0`. Functionally benign: the resource is correct; the
  apply is an idempotent no-op.
- The real cost is **hook re-runs**: a non-empty diff keeps `skipHooks = false`, so a
  StatefulSet app that *also* carries a PostSync hook would re-run that hook on every
  otherwise-no-op sync. No app in the validated stack hits this (the affected service
  has no hooks), which is why it is acceptable to defer — but it is the reason this is
  worth fixing eventually, not merely cosmetic.

# Impact

Scope today: one object in one of ~16 whole-stack apps. The blast radius grows with
any StatefulSet whose VCT leaves `volumeMode` (or another server-defaulted field)
unset and which carries a sync hook.

# Alternatives

- **Server-side diff** (`diff.WithServerSideDiff(true)` + a `ServerSideDryRunner`):
  most accurate, but issues a dry-run apply round-trip **per object per reconcile**.
  In ksync's re-reconciling sync loop over a whole stack (~190 objects) that would
  blow the p50≤2s / p95≤5s budget that is the core design tenet. Rejected.
- **Structured merge diff** (`diff.WithStructuredMergeDiff(true)` +
  `diff.WithGVKParser(clusterCache.GetGVKParser())` + `diff.WithManager(fieldManager)`):
  predicts the SSA merge **locally** from the OpenAPI schema — SSA-aware, no extra
  round-trips. This is the intended fix. It is deferred only because it additionally
  requires the cache to retain `metadata.managedFields` on the cached live objects
  (the field-ownership input it merges against); ksync's cache currently keeps full
  manifests for managed resources but does not explicitly preserve managedFields. That
  plumbing plus re-validating the byte-parity/hook-decision behavior is its own change.
- **Workaround in the chart values** (pin `volumeMode: Filesystem`): fixes the one
  symptom, not the class, and edits the source repo rather than ksync. Rejected as a
  general answer.

# Notes

Fix path, concretely, when picked up: (1) configure the cluster cache to retain
managedFields for managed resources; (2) thread
`WithStructuredMergeDiff(true)/WithGVKParser/WithManager` into the `diff.DiffArray`
call in `internal/engine/sync.go`, guarded on a non-nil parser; (3) extend the engine
tests with a StatefulSet-VCT case asserting a warm re-sync converges to zero
modifications; (4) re-run the whole-stack adoption to confirm every app reaches
`0 applied`.
