---
date: "2026-06-30"
author: "motoki317"
status: "accepted"
---

# Context

ksync renders helm charts against the live cluster: the helm wrapper runs `helm template
--dry-run=server`, the one mode in which `helm lookup` resolves (ADR 20260614-live-cluster-helm-render).
The server-side dry-run has a side effect beyond enabling `lookup`: the apiserver maps every rendered
resource. So an app whose chart ships a CRD together with custom resources of that kind — the shape
ArgoCD deploys in a single Application with `includeCRDs: true` — fails to render with `no matches for
kind` until the CRD already exists in the cluster.

The workaround so far was to split such a chart into two apps: one that installs the CRDs first, one
that renders the rest with a `needs` edge. That diverges from production (ArgoCD renders the whole
chart client-side and applies CRDs before custom resources within one sync) and, when the CRDs are not
exposed by the chart cleanly, tempts vendoring a CRD copy into the repo.

A single `ksync sync` does converge a CRD-bundling app: gitops-engine orders the CRD ahead of the
custom resource and the custom-resource apply succeeds in the same reconcile (verified on docker-desktop
— 23 CRDs + 45 custom resources of those kinds applied and reached Healthy in one sync). What blocked
the single-app shape was only the render-time server-side validation, not the apply.

# Decision

Add a per-app `clientRender: true` field. For that app, ksync renders client-side — `helm template`
with the cluster's discovered capabilities (`--kube-version` / `--api-versions`) but **without**
`--dry-run=server` / `--take-ownership`. Dropping the dry-run is the whole point: it stops the
apiserver validating each resource, so the chart's own custom resources render before their CRDs exist.
This is the way ArgoCD renders.

Mechanically: `render.Options` gains `ClientRenderCommand`, and `Render(dir, clientRender)` selects it.
`cmd/ksync` writes two wrappers (`helmLookupWrapper`, `helmNoLookupWrapper`) — separate files, not one
env-toggled script, because app renders run concurrently and share the process env, so the choice is
made by which path kustomize is handed, never by a mutable env var. Both share the discovered
capabilities and the helm-unavailable guard.

The field affects **render only**. Diff and apply are unchanged: the default server-side diff already
falls back to client-side per resource on a dry-run error (ADR 20260625-server-side-diff-default), so a
custom resource whose CRD does not yet exist diffs correctly without any coupling (verified: a cold
sync with the default diff converges). No per-app diff knob is introduced.

# Consequences

- A chart that bundles its CRDs and custom resources deploys as one app — production/ArgoCD parity —
  instead of a hand-split CRD app plus a `needs` edge, and with no vendored CRD copy.
- The knob is per app and intrinsic to the chart (does this chart need `lookup`?), like `needs` or
  `patches` — not a per-run mode. It composes with the global `--offline-render`: when that is set,
  every app renders with plain helm and no cluster, `clientRender` included.

# Impact

- `clientRender` **disables `helm lookup`** for the app. A chart that uses `lookup` to preserve a
  generated value across syncs (a webhook caBundle, an admin password) will regenerate it each render,
  showing as perpetual drift on `diff` and re-applying it each `sync`. Use `clientRender` only where the
  chart does not depend on `lookup`; if it does, keep the value out of `lookup`'s reach (a fixed value
  in `values`, cert-manager for a webhook cert) or do not set the flag.
- It still needs a reachable cluster (for capability discovery), so it is not a no-cluster mode;
  `--offline-render` remains the escape hatch for rendering without a cluster.
- `Render` gains a parameter; every call site passes `app.ClientRender` (tests pass `false`).

# Alternatives

- **Reuse the global offline render (plain helm, no capabilities) per app.** Rejected: this repo treats
  capabilities as correctness — the wrapper refuses to render rather than use static capabilities, since
  a version-gated template otherwise emits a removed apiVersion that fails to apply (ADR
  20260614-live-cluster-helm-render). An app that renders on every sync with a cluster present should
  keep capabilities. `--offline-render` drops them only because its job is to need no cluster.
- **Name it `lookup: false` or `offlineRender: true`.** `lookup` names the side effect (the lost
  feature), not the render error a user hits, so it is undiscoverable from the symptom. `offlineRender`
  is wrong: this mode keeps capabilities and needs a cluster, so it is not offline. `clientRender`
  parallels the existing `--client-diff` ("client-side instead of a server-side dry-run"); the vocabulary
  is render ↔ diff, with `--offline-render` reserved for the stronger no-cluster case.
- **Couple a per-app client diff to the field.** Rejected as unnecessary: the server-side diff's
  per-resource fallback already handles the cold-CRD case (verified). Fewer knobs.

# Notes

The motivating chart (a VictoriaMetrics k8s-stack with `includeCRDs: true`) renders and converges under
`clientRender`, but its operator subchart uses `lookup` to preserve a generated webhook cert — so under
`clientRender` that caBundle churns each sync. Making that chart churn-free is a chart-configuration
task (point the webhook at cert-manager, or disable the admission webhook in dev), separate from this
decision.
