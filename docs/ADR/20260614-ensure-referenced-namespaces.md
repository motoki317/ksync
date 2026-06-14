---
date: "2026-06-14"
author: "motoki317"
status: "accepted"
---

# Context

ksync auto-creates an app's **own** destination namespace (the `Namespace` in its config), via
gitops-engine's `WithNamespaceModifier(createNamespaceIfMissing)` — ArgoCD `CreateNamespace=true`
parity. That covers the common app, whose resources all live in one namespace.

It does **not** cover an app whose resources span namespaces. A real case: a workflow-engine chart
that provisions execution RBAC (`ServiceAccount`/`Role`/`RoleBinding`) into each *application*
namespace (`team-a`, `shop`, …) so workflows can run there. On a fresh cluster those namespaces do
not exist yet, and gitops-engine creates only the app's own namespace, so the apply fails:

```
.../Role/team-a/...workflow: error running rbacReconcile: ... namespaces "team-a" not found
```

In a fully bootstrapped cluster the owning apps (or a separate bootstrap) have already created
those namespaces, so this only bites when ksync drives the stack onto a bare cluster — which is
exactly the local-dev case ksync exists for. It also coupled apply success to scheduling order
(the cross-namespace app racing the namespace-owning apps), which `needs` cannot sanely express
(it would invert infra-vs-app ordering, and the owning apps may legitimately be excluded).

# Decision

Before applying, `engine.Sync` ensures **every namespace the target resources reference** exists,
not just the app's own. `ensureReferencedNamespaces` collects the distinct namespaces from the
target set (excluding the app's own, handled by the modifier, and the cluster scope) and creates
each if missing.

The created namespaces are **bare and untracked** — no ksync tracking label:

- **Prune-safe.** `isManaged` never matches them, so prune cannot delete a namespace ksync created
  this way (and therefore cannot cascade-delete everything inside it). This is the whole reason not
  to inject `Namespace` objects into the target set: a tracked namespace would be a prune target,
  and removing it from one app's manifest would nuke another app's workloads.
- **Adoptable.** When the app that *owns* the namespace later syncs, `createNamespaceIfMissing`
  sees it already exists and leaves it untouched.

`AlreadyExists` is the steady state and is not an error. The common single-namespace app references
only its own namespace, so the set is empty and this costs **zero** API calls.

# Consequences

- A multi-namespace app applies cleanly on a fresh cluster regardless of scheduling order or which
  other apps are in the run. Validated live: an app provisioning cross-namespace RBAC went from
  failing (`namespaces "…" not found`) to `✓ applied`, creating the referenced namespaces itself.
- No prune-safety regression: created namespaces are invisible to `isManaged`, so destroy/prune of
  the creating app removes only its tracked resources, never the namespaces.

# Impact

- This is a deliberate **divergence from ArgoCD**, which creates only the single destination
  namespace and would itself fail this apply. ksync is a local-dev convergence tool, not an
  Argo controller; auto-creating referenced namespaces removes a fresh-cluster footgun and is
  prune-safe, which is the right trade for the local-dev use case. It is *additive* — single-
  namespace apps behave exactly as before.
- One extra `Namespaces().Create` (tolerating `AlreadyExists`) per *distinct extra* namespace per
  sync. Zero for the common app; a handful for a cross-namespace app, repeated on re-sync. Not
  cached, for correctness if a namespace is deleted out of band — negligible next to render+apply.
- `Engine` gains a `kubernetes.Interface` (built in `New`) for the namespace create; the cache and
  sync paths are otherwise unchanged.

# Alternatives

- **Inject `Namespace` objects into the target set** so gitops-engine applies them first (kind
  ordering puts namespaces early). Rejected: they would carry the tracking label and become prune
  targets — dropping one from a manifest would cascade-delete another app's namespace contents.
- **Require the owning apps to create the namespaces + add `needs` ordering.** Rejected: it inverts
  the dependency (infra waiting on the app layer), and the owning apps may be excluded from a run or
  unable to become healthy (e.g. pending an external bootstrap), which would then block the infra
  app forever under the health gate.
- **Leave it as config (pre-create namespaces in the kustomization).** Rejected for the same
  prune-cascade reason as injecting them, and it pushes a sharp, easy-to-forget edge onto every
  multi-namespace app.
