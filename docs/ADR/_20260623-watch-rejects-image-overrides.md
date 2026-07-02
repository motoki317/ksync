---
date: "2026-06-23"
author: "motoki317"
status: "superseded"
---

> Superseded 2026-07-02 by [20260702-watch-image-override-takeover](20260702-watch-image-override-takeover.md):
> `watch` now **accepts** image overrides with takeover semantics — it seeds an overridden entry from
> the supplied ref on first convergence (like `sync`) but keeps watching its source, and the first
> source edit drops the override and rebuilds. The `--image` flag is defined on `watch` again and a
> set `KSYNC_IMAGE_OVERRIDES` is parsed, not rejected. The contradiction the analysis below names is
> in the *pin-for-the-whole-session* reading only, which remains `ksync sync`'s job; takeover resolves
> it rather than forbidding overrides on `watch`.

# Context

Image overrides (20260616-image-override) let a caller supply a pre-built ref for a `build:` entry
so ksync deploys it instead of building from source. The original decision accepted them on both
`sync` and `watch`, via `--image` and `KSYNC_IMAGE_OVERRIDES`.

For `watch` that is a contradiction in purpose. `watch` exists to run the inner loop — edit source,
rebuild the image, redeploy — and an override does the opposite: it pins a fixed pre-built image and
stops watching that entry's sources. A `watch` session with overrides set silently does no build
work for the overridden images, which is exactly the work the command is for. Worse, the failure is
quiet: a wrapper that exports `KSYNC_IMAGE_OVERRIDES` for its `sync` path and then also runs `watch`
in the same shell gets a watch loop that ignores source edits with no indication why.

# Decision

`watch` does not accept image overrides.

- The `--image` flag is **not defined** on `watch`, so passing it is a flag error.
- If `KSYNC_IMAGE_OVERRIDES` is set (non-blank), `watch` **fails fast** at startup — before it
  connects to the cluster — with a message that names the variable and points to `ksync sync`.

`sync` is unchanged: it still accepts both forms. Overrides remain a deploy-time, sync-only
affordance.

# Consequences

- The contradiction is impossible to enter by accident: a set override env aborts `watch` with a
  clear reason instead of producing a loop that quietly skips builds.
- The split is honest about the two commands' jobs — `watch` builds from source, `sync` deploys what
  it is given (built or supplied).
- A wrapper that shares one shell between a `sync` deploy and a `watch` session must scope the env
  var to the `sync` invocation (it already constructs that command); the fail-fast makes the
  requirement obvious the first time.

# Impact

- `cmd/ksync`: `runWatch` drops the `--image` flag and the `imageOverrides` call, adds the
  `KSYNC_IMAGE_OVERRIDES` guard, and passes no overrides to the loop. `internal/loop` keeps
  `Options.Overrides` (still used by `sync`); only the `watch` command stops populating it.
- Docs: `docs/usage.md` override section marks overrides `sync`-only and documents the `watch`
  rejection.
- Tests: `TestRunWatch_RejectsEnvOverrides`, `TestRunWatch_RejectsImageFlag`.

# Alternatives

- **Silently ignore overrides in `watch`.** The status quo. Rejected: a watch loop that does no
  build work for an overridden image, with no signal, is the surprising behavior this fixes.
- **Honor overrides in `watch` (build the rest, pin the overridden).** Coherent in principle, but it
  asks the inner-loop command to also be a partial deploy engine; the two roles are cleaner split
  across `sync` and `watch`, and no use case asked for the mixed mode in a watch session.
- **Warn but continue.** Rejected: a warning in a long-running loop scrolls away, and the user's
  intent (override on a build-from-source command) is genuinely ambiguous — failing fast forces the
  choice instead of guessing.

# Notes

The guard checks the env var directly rather than parsing it, so even a malformed value aborts; the
goal is "overrides were requested on `watch`," not "valid overrides were requested."
