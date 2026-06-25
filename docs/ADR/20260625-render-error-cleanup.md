---
date: "2026-06-25"
author: "motoki317"
status: "accepted"
---

# Context

ksync renders by default against the live cluster (capability discovery): the helm
inflation step kustomize runs is `helm template --dry-run=server`, so helm `lookup`
and version-gated templates resolve. Against a fresh cluster the chart's CRDs may
not exist yet, and helm fails the server-side resource mapping. kustomize
(`HelmChartInflationGenerator.runHelmCommand`) then wraps that failure as

```
<helm stderr>: unable to run: '<helm> <args>' with env=[...] (is '<helm>' installed?): exit status N
```

and krusty returns it unwrapped, so the user sees the whole string. The signal —
which kind could not be mapped — is buried under a temp-path command dump, the helm
env, and an `(is '<helm>' installed?)` tail that is actively misleading: the helm
binary is ksync's own temp wrapper, always present. The user asked for an error
that is actionable at a glance.

# Decision

`internal/render` intercepts the krusty render error (`cleanRenderError`, wrapping
the `krusty.Run` result before the `rendering <dir>:` prefix) and, when it carries
the kustomize helm-exec signature, strips the command/env/"installed?" noise to
recover helm's own stderr. For the common unmappable-kind failure
(`no matches for kind "X" in version "Y"`) it replaces the message with the kind
and the real fixes:

```
cluster cannot map X (Y).
fix: if it is a custom resource, install its CRD first (apply the base layer); or render offline: --offline-render (helm `lookup` results will be empty)
```

The CRD fix is named first because a missing CRD is the dominant cause, but
phrased conditionally: the same helm error also covers a built-in apiVersion the
cluster does not serve, so the message must not flatly assert "no CRD".
`--offline-render` is the universal escape hatch, offered with its standing caveat
that `lookup` results will be empty. Offline render resolves this failure class by
construction: plain `helm template` does no server-side resource mapping, and
`lookup` returns empty rather than failing.

The cleanup is **fail-safe by degrade**. No helm-exec signature → the error is
returned unchanged. Signature present but only exec noise (empty helm stderr), or
an unrecognized inner pattern → the raw error or helm's own stderr respectively,
never an empty or half-built message. So a future helm/kustomize wording change
drops back to the raw error rather than breaking. The
cleaned error keeps the raw one reachable via `Unwrap`, so `errors.Is`/`-v` still
reach the full helm invocation.

# Consequences

- A missing-CRD render failure (the first thing a `diff`/`sync`/`render` against a
  fresh cluster hits) reads as one problem statement and two fixes, not a wall of
  temp paths.
- The fix applies in `internal/render`, so every command that renders (`diff`,
  `sync`, `render`, `images`, `watch`) benefits without per-command code.
- The misleading `(is X installed?)` tail — which would send a user to check a helm
  install that is fine — is gone for every helm failure, not only the CRD case.

# Impact

- The cleanup parses a dependency's error string by regexp, which is inherently
  coupled to kustomize/helm wording. The fail-safe degrade bounds the blast radius:
  the worst outcome of a wording drift is the current (raw) behavior, not a wrong
  or empty message. The signature and the kind/version pattern are pinned to the
  kustomize release the module already pins.
- Only the `krusty.Run` error is cleaned; the post-render wraps
  (`objectsFromResMap`, `configImageFieldSpecs`) are not helm-exec failures.

# Alternatives

- **Leave the error as-is, document the noise** — zero code, but the first error a
  new user hits stays cryptic, which is exactly the reported complaint.
- **Detect missing CRDs ourselves and skip live render** — would need to enumerate
  the cluster's CRDs and diff against the rendered kinds before rendering; far more
  machinery, and it would silently change rendered output (offline vs. live) instead
  of telling the user to choose. Rejected: the user should decide between installing
  the CRD and rendering offline.
- **Special-case the message in `cmd/ksync` (main.go)** — would have to re-parse the
  string after two `%w` wraps and would only fix the CLI, not other render callers.
  Cleaning at the source keeps the typed error and one implementation.

# Notes

- Fixtures in `rendererror_test.go` use invented identifiers (`Widget`,
  `example.com/v1alpha1`, app `shop`) so no live cluster name reaches a tracked
  file (leakcheck rule).
