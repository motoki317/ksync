---
date: "2026-06-25"
author: "motoki317"
status: "accepted"
---

# Context

`ksync diff` and `sync`/`watch`'s apply-skip both computed their diff client-side
(gitops-engine's in-process three-way / structured-merge `diff.Diff`). A client-side
diff predicts the post-apply object by merging the target into live; it cannot know
that the apiserver will *drop* a field. The reference case: a StatefulSet's
`spec.updateStrategy.rollingUpdate.maxUnavailable` is gated behind the alpha
`MaxUnavailableStatefulSet` feature gate, disabled by default, so the apiserver
prunes it on write. The rendered manifest carries the field, live never does, and:

- `ksync diff` shows `+maxUnavailable` as a pending change on every run, surviving
  every sync — a perpetual phantom diff.
- `sync`'s apply-skip (`WithResourceModificationChecker`, fed by `diff.DiffArray`)
  sees the resource as out of sync every time and re-applies it on every sync.

`ADR 20260625-diff-command` deferred server-side diff for exactly this, citing the
per-resource API round-trip and divergence from sync's own apply-skip. This
supersedes that deferral.

# Decision

Default **both** `ksync diff` and the `sync`/`watch` apply-skip to a server-side
diff: a dry-run server-side apply whose predicted result reflects the apiserver's
defaulting and pruning. `--client-diff` opts back into the in-process diff (on
`diff`, `sync`, and `watch`).

- **Engine.** A `differ` (`internal/engine/serverdiff.go`) wires gitops-engine's
  `diff.WithServerSideDiff` to the dry-run applier from
  `kube.ManageServerSideDiffDryRuns` and the GVK parser the warm cache already
  maintains (`GetGVKParser`). It is the dedicated server-side-diff applier, **not**
  `ManageResources`: the latter's printer emits kubectl's "serverside-applied
  (server dry run)" status line, which fails to unmarshal as the predicted object
  and silently degrades every resource to client-side. A `noiseNormalizer` strips
  status/managedFields from the server's predicted-live (gitops-engine's default
  normalizer is a no-op).
- **Per-resource fallback.** A dry-run that errors for one resource (a validating
  webhook, missing RBAC, an SSA field-manager conflict, a transient API error)
  falls back to the client-side result for *that* resource, so one resource never
  fails the whole diff or sync.
- **Sync cost containment.** The server-side decision runs on the **first reconcile
  only** — the iteration that picks the apply set. The health-wait polls that follow
  reuse the cheap client-side `diff.DiffArray`, since the apply set is already
  decided and a dry-run per poll would cost an API round-trip per resource per
  second.

# Consequences

- The `maxUnavailable`-class phantom is gone: `ksync diff` reports the resource in
  sync, and `sync` no longer re-applies it (validated live: 0 applied server-side
  vs 1 applied with `--client-diff`).
- `diff` reports what `sync` would apply by construction again — both read the same
  strategy, so the preview matches the apply.
- Server-side diff is **more truthful, not uniformly quieter.** It predicts what
  SSA would actually do, so it surfaces real changes the client-side three-way
  merge hid — e.g. an empty sub-object an operator co-manages that the rendered
  manifest does not declare, which SSA would remove. Such a resource now shows in
  `diff` and re-applies under `sync` (an apply the co-managing operator may undo).
  A per-field ignore (ArgoCD-style `ignoreDifferences`) is the targeted remedy for
  that noise and is deferred to a follow-up.

# Impact

- **API cost.** A dry-run server-side apply per resource on the first reconcile.
  ArgoCD measured per-application reconcile rising from ~0.5s to >5s when enabling
  server-side diff on its *controller loop*; `ksync` confines it to the apply-set
  decision (one-shot for `diff`, first-reconcile for `sync`) rather than every
  poll, but it is still N round-trips against the p50 ≤ 2s budget. Accepted as the
  chosen fidelity-over-latency trade; `--client-diff` is the escape hatch for a
  tight edit loop.
- `kube.ManageServerSideDiffDryRuns` writes a temporary kubeconfig per call; the
  differ is built once per `Diff`/`Sync` and its cleanup removes the temp file.
- The server-side path and its fallback are validated end-to-end against a live
  cluster, not in a unit test (the dry-run needs an apiserver). The pure pieces —
  classification, array alignment/aggregation, the label-by-context rule — keep
  their cluster-free tests.
- **First-reconcile-only is not a full guarantee for multi-step syncs.** A resource
  held behind an earlier hook or sync wave is classified server-side on the first
  pass, but a later pass reclassifies it client-side and may apply it anyway — so a
  server-pruned field can still re-apply within a hook/wave sync. The common case (a
  no-hook app decides and completes on the first reconcile) gets the full benefit;
  the multi-step case degrades to an idempotent server-side-apply no-op, not
  incorrectness. Tightening it would mean carrying the first decision forward by
  resource key or caching server-side results across polls — deferred.

# Notes (implementation)

- The ksync tracking label is stripped by **context, not strategy**: the diff
  *display* strips it so a label-only adoption is not shown as a change, but the
  sync *apply-set decision* keeps it, so adopting an unlabeled-but-matching resource
  reads as modified and is applied (and thereby owned). A shared normalizer that
  stripped it for both would skip the adopting apply — the resource would never
  become ksync-managed and prune would never see it.
- Secret values are masked for display except where gitops-engine's server-side
  diff already masked them — its *update* path, not its shallow create/prune diff —
  so the display-masking gate is "server-side diff of an existing resource", not
  "server-side was used".
- The sync apply-decision compares a core Secret's *masked* values when it runs
  server-side (`serverSideDiff` masks predicted-live and live before computing
  `Modified`). A changed value still applies because `HideSecretData` assigns
  placeholders by order of first appearance — distinct values get distinct-length
  `+` runs regardless of byte length — so any value change survives masking as a
  diff. This injectivity is load-bearing here: a masking that collapsed distinct
  values to one placeholder would silently skip applying a rotated secret of equal
  length.

# Alternatives

- **Keep client-side default, add `--server-diff` opt-in** — lowest risk, but
  leaves the misleading phantom as the default experience; the whole point is to
  fix the default.
- **`ignoreDifferences` per field instead of server-side** — zero API cost and
  targeted, but only as good as the enumerated field list; it would not catch the
  next defaulted/pruned field. Complementary, not a replacement; deferred.
- **Server-side on every reconcile (not just the first)** — simplest code, but N
  dry-runs per second during a health-gated wait; rejected for the obvious latency.

# Notes

- Supersedes the "ServerSideDiff (dry-run apply per resource)" rejection in
  `ADR 20260625-diff-command`.
