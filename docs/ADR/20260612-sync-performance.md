# Sync performance: client rate limits and out-of-sync-only apply

Date: 2026-06-12
Status: accepted

## Context

The watch loop's value is the change→applied latency (target: p50 ≤ 2s, p95 ≤ 5s per
single-app edit). Small fixture apps met it trivially; a large public chart did not. The
benchmark app was `victoria-metrics-k8s-stack` 0.74.1 (105 rendered objects, 2.7MB YAML,
23 CRDs — several hundred KB each) inflated through the standard kustomize `helmCharts` shape,
synced to a local docker-desktop cluster.

Measured baseline (before any tuning):

- cluster-cache warm-up: 19–24s, almost entirely `client-side throttling` waits in discovery.
- save→applied in watch mode: **6.4s** — ~0.9s render + ~5.5s applying all 101 non-CRD-task
  resources serially at ~55ms each, even when nothing changed.
- gitops-engine runs all apply tasks concurrently (`processCreateTasks` spawns one goroutine
  per task), so the serialization had to come from below: client-go's default rate limiter
  (QPS 5, burst 10) on the shared `rest.Config`.

## Decision

1. **Raise the kube client rate limits to QPS 50 / burst 100** in `engine.RESTConfig` — the
   same defaults ArgoCD uses for its kube clients (`K8sClientConfigQPS`). A long-running
   local-dev tool pointed at its own local cluster does not need client-side self-throttling.
2. **Diff before sync, apply only what differs.** `Engine.Sync` fetches the app's live
   objects from the warm cluster cache (`GetManagedLiveObjs`), diffs them against the rendered
   target (`pkg/diff.DiffArray`), and passes the result to the engine as
   `WithResourceModificationChecker` — ArgoCD's ApplyOutOfSyncOnly behavior. A no-change sync
   applies nothing; an edit applies exactly the changed resources.
3. **Serialize the multi-document YAML lazily.** Only `ksync render` needs it; building 2.7MB
   of YAML cost ~170ms on every watch-loop sync.

Measured after:

- warm-up 19.4s → **0.8s**; one-shot `ksync sync` (no changes) ~30s → ~3s.
- save→applied (no-op or one-field edit): 6.4s → **1.2–1.3s**, dominated by rendering
  (~0.5s krusty + ~0.27s helm subprocess + ~0.1s object decoding).
- destroy of 101 resources: ~1s.

## Consequences

- `engine.Sync` and gitops-engine each run one `DiffArray` per sync (the engine diffs
  internally to decide hook skipping and cannot consume ours). At ~105 objects the double
  diff is not measurable next to render time; revisit only with data.
- Resources whose rendered form genuinely changes every render — e.g. charts that mint a
  fresh self-signed webhook cert per `helm template` — are correctly re-applied every sync.
  That is the same behavior such charts get under ArgoCD; an `ignoreDifferences` equivalent
  is deliberately out of scope until needed.
- Faster concurrent applies exposed a first-install ordering race: CRDs and their CRs land in
  the same sync wave, and CR applies can beat CRD establishment ("server could not find the
  requested resource"). Under the old rate limiter the trickle of requests hid this. The
  failure is surfaced as a sync error; watch mode's retry converges on the next attempt,
  matching ArgoCD, which also leaves CRD/CR ordering to retries or explicit sync waves.
  Automatic CRD waving was considered and rejected: it would diverge from ArgoCD semantics.
