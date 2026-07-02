---
date: "2026-07-02"
author: "motoki317"
status: "accepted"
---

# Context

Image overrides (20260616-image-override) let a caller supply a pre-built ref for a `build:`
entry so ksync deploys it instead of building from source. 20260623-watch-rejects-image-overrides
then made `watch` refuse them: the `--image` flag was undefined and a set `KSYNC_IMAGE_OVERRIDES`
failed the command fast.

That rejection solved one specific problem — a wrapper that exports `KSYNC_IMAGE_OVERRIDES` for its
`sync` path and then runs `watch` in the same shell would otherwise get a loop that silently does no
build work for the overridden images. But it also cut off a coherent, useful mode. A developer
starting a `watch` session commonly wants the stack up *now* from pre-built images (a `just stack::up`
that has already resolved every tag), and then to iterate on the one or two services they actually
edit. Under the rejection they cannot: they must choose between `sync` (up fast, but no loop) and
`watch` (a loop, but every image builds from source at startup, including the dozen they will never
touch this session).

The rejected design considered only "pin the override for the whole watch session" (the `sync`
semantics carried verbatim into `watch`), which is indeed a contradiction — a watch loop that never
watches. But there is a second reading the earlier ADR did not separate out: **seed** from the
override, then hand the image back to the loop the moment its source is edited. That keeps `watch`'s
fast start *and* its inner loop, and it is what this ADR adopts.

# Decision

`watch` accepts image overrides with **takeover** semantics — the single, default behavior (no
opt-in flag).

- **Same input as `sync`.** `watch` defines the same repeatable `--image IMAGE=REF` flag and reads
  the same `KSYNC_IMAGE_OVERRIDES` env var, parsed by the shared `imageOverrides` path (a flag wins
  over the env per image; an override naming no build entry is dropped with a logged note).
- **Seed, don't pin.** On first convergence an overridden entry is **not built** — the supplied ref
  is injected at deploy, exactly as one-shot `sync` does. This is the fast start.
- **Still watched.** Unlike the rejected pin, an overridden entry's sources **are** watched. The
  first source change to that entry **drops the override** and rebuilds from source; every deploy
  after that injects the freshly built tag. The watched image has "taken over" from the supplied ref
  and rejoined the inner loop.
- **Prompt first, then take over.** With the interactive build gate (the default on a terminal), a
  source change to an overridden entry is held like any other and shown in the prompt; the override
  is dropped only when the user *confirms* that build. Under `--auto` (or a non-terminal) the change
  takes over immediately, as an ordinary auto-rebuild would.
- **A manifest edit does not take over.** Editing an overridden app's manifests (not its build
  source) redeploys and re-injects the still-standing override — only a *source* change takes over.

`sync` is unchanged: its overrides still pin for the whole one-shot run. Pinning an image for an
entire session — deploy the supplied ref and never build it — remains `ksync sync`'s job, not a
`watch` mode.

## `resyncAll` leaves overrides intact

`watch`'s full-resync path (the manual `Resync` trigger, and the identical recovery after a dropped
fsnotify event) re-dirties every buildable entry so a possibly-missed source edit still reaches the
cluster. It deliberately **skips overridden entries**: takeover is triggered only by a real, observed
source change, never by a blind resync. So a resync redeploys a still-overridden entry's supplied ref
rather than dropping it — a dropped-event recovery must not silently promote an override to a
from-source build the user never asked for. (Whether to schedule a rebuild is recomputed from each
entry's *current* override state, because takeover changes that state at runtime: an entry that has
already taken over is rebuilt on resync, one still overridden is not.)

# Consequences

- `watch` and `sync` share one first-time behavior: overridden entries deploy the supplied ref and
  build nothing. A wrapper can hand the same `KSYNC_IMAGE_OVERRIDES` to both without special-casing.
- The fast-start / inner-loop tension is resolved without a mode flag: the developer gets the stack
  up from pre-built images and, on the first edit of any service, that service silently rejoins the
  build-from-source loop. No command to re-run, no override to unset.
- The old accidental-contradiction case is no longer a footgun *because* it is no longer a
  contradiction: a shared-shell `watch` with overrides set now does the sensible thing (seed, then
  take over on edit) instead of either silently skipping builds or aborting.

# Impact

- `cmd/ksync`: `runWatch` drops the `KSYNC_IMAGE_OVERRIDES` guard, gains an `images` parameter, and
  resolves overrides via the shared `imageOverrides` into `loop.Options.Overrides`; `newWatchCmd`
  defines the `--image` flag (same shape as `sync`/`diff`).
- `internal/loop`: the three "skip overridden entries" guards (scope derivation, the change→build
  mapping, and the watched roots) are removed, so overridden entries are watched. A source change
  clears `buildState.override` (immediately under `--auto`, at gate release otherwise) and dirties
  the entry. **Structural fix:** every app that declares a build is now registered in the build
  scheduler and requires a build func, even one whose every entry is currently overridden — because a
  later takeover must rebuild it, and `schedule.MarkDirty` on an unregistered app is a silent no-op
  that would gate the app's deploy forever. Startup still builds only non-overridden entries, so a
  fully-overridden app is registered but not built at start.
- Docs: `watch`'s help (`cmd/ksync/help.go`) and the `builds` topic now describe takeover instead of
  the rejection.
- Tests: the two rejection tests become acceptance tests (`--image` is defined; a set env is parsed,
  not refused). `internal/loop` gains takeover tests, including a **fully-overridden** app whose
  takeover build only schedules because of the registration fix (the regression guard for that fix),
  and a gate-release-drops-override test.

# Alternatives

- **Keep the rejection (20260623).** Rejected: it forecloses the common "up fast from pre-built,
  then iterate on a few" session. The contradiction it feared was only in the *pin* reading; takeover
  removes the contradiction rather than the feature.
- **Pin the override for the whole watch session (the original 20260616 behavior on `watch`).**
  Rejected — this is the genuine contradiction 20260623 identified: a watch loop that never watches
  the overridden image. If a developer truly wants a fixed image for a whole session, `ksync sync`
  already does exactly that.
- **A separate `--always-override-image` (or similar) flag to choose pin vs. takeover.** Rejected:
  two override modes on `watch` is one knob too many. Pin-forever already has a home (`sync`); on
  `watch`, takeover is the only behavior that keeps the command's promise (an inner loop), so it is
  the single default.
- **Take over on any change, including manifest edits.** Rejected: a manifest edit does not touch the
  image, so promoting the entry to a from-source build on one would surprise the user and rebuild an
  image whose source never changed. Only a change under the build's watched scope takes over.

# Notes

Supersedes 20260623-watch-rejects-image-overrides. The override *parsing and injection* mechanism is
unchanged from 20260616-image-override; this ADR changes only what `watch` does with an overridden
entry after the first deploy (watch its source; take over on edit) and which apps the build scheduler
must know about (all build-declaring apps, so a takeover can schedule).
