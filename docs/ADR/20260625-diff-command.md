---
date: "2026-06-25"
author: "motoki317"
status: "accepted"
---

# Context

`ksync diff` was reserved as a command name from day one but left unimplemented. It is the
read-only preview of `ksync sync`: render the selected apps, compare against live cluster state,
and show what a sync would change — without applying anything. The contract has to answer three
questions precisely: what exactly is compared, how differences are computed, and how the
`build:` image story (which `diff` cannot resolve, since it does not build) is handled.

# Decision

`ksync diff [apps...]` renders each selected app exactly as `sync` would (post-render
`patches`, then image injection) and prints a per-resource unified YAML diff against live state.
It is read-only: no apply, no build, no hook execution.

**Diff reports X ⟺ sync would apply X.** The command reuses sync's own pre-diff pipeline rather
than a parallel one, so the preview cannot drift from the apply:

- `StampTracking` + `fillDefaultNamespace` on the target (sync stamps the tracking label and
  fills the default namespace before any key matching; skipping either fabricates a spurious
  label diff or mis-keys every namespaced resource as create+prune).
- `GetManagedLiveObjs` for the live map, then `sync.Reconcile(target, live, namespace, cache)` —
  the same call sync uses. Reconcile aligns each target with its live counterpart, splits hooks
  out, applies the unknown-scope namespace fallback and live dedup, and appends live-only managed
  resources as `Target=nil` pairs (the prune candidates).
- Per pair, `diff.Diff(target, live, diff.WithLogr)` — the three-way / structured-merge engine
  sync's apply-skip checker already uses (`diff.DiffArray` in `Sync`). Create/update/prune is
  classified **structurally** from the reconciled pair's nil-ness, not from `DiffResult.Modified`
  (which is `false` for a deletion-shaped `Diff(nil, live)`).

**Hooks are excluded** (`hook.IsHook` on either side). A hook is applied by sync's hook machinery,
not the normal resource pass; a lingering live hook Job (a `BeforeHookCreation` delete-policy
Job from the last run) would otherwise mis-report as a prune. Hook reruns are a sync side effect
`diff` does not model in v1.

**Secrets are masked for display only; classified on real data.** `diff.Diff` alone base64-encodes
`stringData` into `data` (`NormalizeSecret`) but does not redact, so an unmasked Secret diff would
print its (trivially decodable) values to the terminal — `diff.HideSecretData` replaces each `data`
value with a `+`-run. Create/update classification runs on the **unmasked** normalized objects, and
only the displayed `Before`/`After` are recomputed on masked copies, so sync-fidelity rests on the
real data rather than on the masking. `HideSecretData` happens to preserve a value-only change —
it maps distinct values to distinct `+`-run lengths, so the change still renders a valueless hunk
(`password: ++++++++` ⟶ `password: ++++++++++++`) — but that is a property of a display helper, and
classifying on the masked form would couple "diff ⟺ sync" to it. Masking is gated to core/v1
Secrets — a ConfigMap (also a `data` map) keeps its values.

**Build images: carry the live dev tag forward, fail open.** `diff` does not build, so for an
un-overridden `build:` entry the rendered target carries the kustomization's base ref while live
runs the last `:ksync-<12hex>` content-addressed tag — a spurious image diff on every build app.
The command reads each build repo's current live ref and, **only when it is a recognized
`ksync-<12hex>` dev tag**, injects it into the target through `Result.SetImages` — the same
field-spec-aware path sync uses, so the rewrite reaches CRD-embedded image fields declared via
the kustomization's `configurations:` (hard-coding the standard container paths would miss them)
and applies the identical `Always`→`IfNotPresent` pull-policy fix. The target's image fields then
match live and drop out of the diff; a note names the carried repos.

A repo with no managed live image, an inconsistent set, or a non-ksync tag is left untouched — its
real diff shows, but at the kustomization's **base ref** (`:main`), which is not what a sync would
deploy (sync rebuilds to a fresh `ksync-<12hex>` tag). A separate note names these repos and warns
the displayed ref is the source ref, not the deploy ref — material because on a cluster mid-migration
to ksync (every build image still at a foreign `dev-<…>` tag) carry-forward fails open on *every*
build app, so without the note every `:main` line reads as literal. An app with a fail-open build
image is **never reported in sync** even when its rendered image happens to equal live: a sync would
still rebuild and inject a fresh tag the diff cannot predict, so its section prints to carry the
note and the global "No changes" line is suppressed. A `build:` entry whose image **no rendered
resource references** (an orphaned entry) is dropped first — it would otherwise emit a phantom note
and suppress a genuine all-in-sync result though a sync would change no image.

Image overrides (`--image IMAGE=REF`, `KSYNC_IMAGE_OVERRIDES`) are accepted as on `sync` and
injected as real refs (not suppressed), so `diff` previews an overridden sync faithfully.

v1 exits 0 and only displays; a CI-gating `--exit-code` is deferred.

# Consequences

- The preview is faithful to apply by construction: the same stamp, namespace fill, reconcile,
  and diff engine sync runs. A reviewer can read `diff` before a `sync` and trust it.
- Build-heavy stacks (the reference config has many `build:` apps) show only real spec changes,
  not per-build dev-tag churn, while never hiding a manually-drifted or non-ksync image.
- The pure core (`diffResources` over a `ReconciliationResult`, and the live-ref collector) is
  unit-tested without a cluster, matching the existing `pending`/`hasDegradedHook` test seams.

# Impact

- Not a complete server-side-apply diff. Sync applies with SSA but, like sync's own apply-skip
  checker, `diff` uses the client-side three-way / structured-merge engine; without a last-applied
  annotation, an SSA field removal can be missed. True SSA-fidelity diff would change `Sync` and
  `diff` together and is out of scope. Documented, not silently implied. **(Superseded by ADR
  20260625-server-side-diff-default: both `diff` and sync's apply-skip now default to a server-side
  dry-run diff, with `--client-diff` to opt back into this client-side path.)**
- Build-tag carry-forward shows the *current* deployed image, so a source edited but not yet
  synced does not appear as an image change in `diff` (diff cannot know the new content hash
  without building). The note states this.
- Adds `github.com/pmezard/go-difflib` as a direct dependency (already present indirectly via the
  k8s/test stack), for unified-diff rendering — no new module download.

# Alternatives

- **`diff.ServerSideDiff` (dry-run apply per resource)** — most faithful to what SSA changes, but
  a per-resource API round-trip and a gvkParser/openapi dependency, and it would diverge from
  sync's own apply-skip decision. Rejected for v1; revisit only alongside a sync-side SSA-diff
  change. **(Adopted in ADR 20260625-server-side-diff-default — as the new default for both `diff`
  and sync's apply-skip together, which is what kept them from diverging.)**
- **Align with `alignedLiveObjs` instead of `sync.Reconcile`** — drops live-only resources, so it
  cannot surface prune candidates and misses reconcile's unknown-scope / dedup handling.
- **Leave build-image diffs as-is (document the noise)** — zero logic, but a guaranteed spurious
  image diff on every build app on every run; the reference stack would be unreadable.
- **Symmetric sentinel rewrite of target *and* live image tags** — also field-spec-correct, but
  requires rewriting live objects through a reconstructed resmap; carry-forward writes only the
  target through the existing `SetImages` and reads live, with fewer moving parts and the bonus
  that the target becomes byte-identical to what sync last applied.

# Notes

- `diff` renders live by default (capability discovery), like `render`/`sync`/`images`;
  `--offline-render` opts out (helm `lookup` charts will not resolve).
