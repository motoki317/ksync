---
date: "2026-06-24"
author: "motoki317"
status: "accepted"
---

# Context

The sync-health gate (ADR 20260614-sync-health-gate) makes a `Sync` mean *deployed and serving*,
so a `needs` edge waits for its dependency. But the gate is only as good as the health it can read,
and gitops-engine has **no built-in health check for custom resources**. `health.GetResourceHealth`
returns `(nil, nil)` for any kind it does not recognise, which both the wave gate
(`syncContext.Sync`) and ksync's own post-apply gate read as *no health check → ready once present*.

So a custom resource counts as healthy the instant its object exists — before its controller has
done anything. A later-wave consumer of an operator-managed dependency (an ECK `Elasticsearch`)
then races the dependency's actual readiness: the CR is "present" in milliseconds, the wave
advances, and the consumer starts against a cluster that is still forming. The gate from the prior
ADR is real but blind to exactly the resources whose readiness takes longest and matters most.

ArgoCD solves this with per-kind `resource_customizations` health scripts (Lua). ksync needs the
same: a way to teach the engine how to read a CR's controller-maintained `.status`.

# Decision

Add `resourceHealth`, a `health.HealthOverride` keyed by `GroupKind`
(`internal/engine/health.go`), and pass it **everywhere ksync reads health** — both
`sync.WithHealthOverride` (the wave gate) and every `health.GetResourceHealth` call in the
post-apply gate and degraded-resource reporting. Each map entry **ports the matching ArgoCD
`resource_customizations` health.lua to Go**, so ksync's wave ordering matches ArgoCD's. An unmapped
`GroupKind` returns `(nil, nil)` and falls through to the built-in checks — the same contract as an
empty Lua script. Adding a kind is adding one map entry.

The first entry, ECK `elasticsearch.k8s.elastic.co/Elasticsearch`, is a faithful port: it reads
`status.availableNodes` against the summed `spec.nodeSets[].count` (Progressing until every desired
node is up), then classifies on `status.phase` + `status.health` colour — `Ready`+`green` → Healthy,
`yellow` → Progressing, `red`/`Invalid` → Degraded, `ApplyingChanges`/`MigratingData` → Progressing,
anything else → Unknown. gitops-engine advances a wave only on Healthy, fails on Degraded, and keeps
waiting on Progressing/Unknown — so the consumer waits for the cluster to actually be green.

The post-apply gate's `unhealthyStatus` now also **fails closed**: an assessment *error* (not just
`nil`) is reported as holding the gate (Unknown), rather than the previous behaviour of treating an
error as healthy and letting the resource pass silently.

# Consequences

- A later-wave consumer of an ECK `Elasticsearch` waits until the cluster is green before it
  deploys. Validated end-to-end on a cold single-node cluster: the consumer's pods appeared only
  on the first poll where the CR read `Ready/green`, never before — and zero consumer pods
  crash-looped, where previously they raced the forming cluster.
- The override is one place, reused by the wave gate and the post-apply gate, so a CR's health is
  assessed identically no matter which gate observes it.
- The behaviour tracks ArgoCD: the same manifests gated the same way under both tools, which is the
  project's parity goal.

# Impact

- The classification is table-tested (`health_test.go`) against each phase/colour and the
  node-count-not-yet-met path; the end-to-end wait is covered by cold-cluster reproduction (the
  engine package has no apiserver harness).
- New CR kinds are unsupported until ported, but they degrade safely to the built-in fall-through
  (present → ready) — the prior behaviour — rather than erroring. The map documents what is gated.

# Alternatives

- **Embed a Lua interpreter and run ArgoCD's scripts verbatim.** Rejected: a heavy dependency to
  reproduce a handful of `.status` reads. Porting the few kinds ksync gates to Go is smaller, typed,
  and testable; the GroupKind map mirrors ArgoCD's per-kind layout so the correspondence stays clear.
- **Special-case the Elasticsearch read inline in the gate.** Rejected: the `HealthOverride`
  interface is gitops-engine's own extension point and is what the wave gate consumes, so a map of
  overrides composes with both gates for free and generalises to the next CR kind.

# Notes

A **single-node** development Elasticsearch stays permanently `yellow` once an application creates
indices with replicas (a replica shard has no second node to land on). This override deliberately
maps `yellow` → **Progressing**, not Healthy — matching ArgoCD, and correct for a multi-node
production cluster where lingering yellow is a genuine not-yet-converged signal. Making a single-node
dev cluster reach green is therefore an **application-side** concern (configure index replicas to 0
for that environment), not a reason to weaken the port — special-casing yellow→Healthy here would
mask a real degraded state on production.
