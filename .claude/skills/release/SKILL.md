---
name: release
description: >-
  Use when cutting a ksync release — publishing a new version of the ksync CLI, or when the user
  says "release X.Y.Z", "tag a release", "ship a new version", or "cut the first release". ksync has
  ONE release track: pushing a `vX.Y.Z` git tag fires both GoReleaser (GitHub Release) and the cachix
  pre-build. This skill is the runbook — the pre-flight gates, the exact order, and the traps that
  leave a release half-published. Reach for it whenever a git tag is about to trigger a public artifact.
---

# Cutting a ksync release

ksync has **one release track**, driven by pushing a `vX.Y.Z` git tag. The authoritative summary is
AGENTS.md "Releases" — read that for the *what*; this skill is the *runbook* for doing it without
leaving a release half-done. (Contrast kd, which has two tracks; ksync has no chart and no
`Chart.yaml` version-match gate. Do not copy a chart step from muscle memory.)

| Trigger | Workflow | Publishes |
| --- | --- | --- |
| `vX.Y.Z` tag push | `.github/workflows/release.yaml` (GoReleaser, `.goreleaser.yaml`) | GitHub Release: linux/darwin × amd64/arm64 tarballs (each with the binary + LICENSE + README) + `checksums.txt` + grouped changelog |
| the **same** tag push | `.github/workflows/cachix.yaml` | `nix build .#ksync` on x86_64-linux, aarch64-linux, aarch64-darwin → pushed to the `motoki317-ksync` cachix cache, so `nix run github:motoki317/ksync` substitutes the binary |

**One tag, two workflows. Both must go green** — verifying only the GitHub Release misses a broken
cachix push (which silently degrades every downstream `nix run`/`nix develop` to a from-source compile).

## Key facts that shape the flow

- **The version comes from the tag, NOT a file.** `main.version` (cmd/ksync/main.go) defaults to
  `"dev"`; GoReleaser stamps it from the tag (`-X main.version={{ .Version }}`) and the flake stamps
  it from `self.shortRev`. There is **no source version constant to bump** — do not go hunting for
  one. The tag *is* the version.
- **Known quirk, not a bug:** the GoReleaser binary's `ksync version` prints `X.Y.Z`, but a
  `nix run github:motoki317/ksync/vX.Y.Z -- version` build prints the git short-rev (the flake stamps
  the rev, not the tag). Expected; don't "fix" it mid-release.
- **The changelog is generated from commits.** `.goreleaser.yaml` `changelog`: scope-aware excludes
  (`docs`/`test`/`chore`/`ci`, e.g. `^docs(\(.+\))?!?:` so a scoped `docs(readme):` is dropped too),
  grouped Features/Bug fixes/Performance/Other. So conventional-commit subjects ARE the public release
  notes — they must read well *before* you tag. `use: github` auto-falls-back to `git` on the first
  tag (no previous tag to compare); that fallback is normal, not an error.
- **Tags are lightweight, on the commit you want released.** Annotated vs lightweight is irrelevant
  to GoReleaser (the changelog is from commits, not the tag message). Match the repo's first release
  (`v0.1.0`, lightweight).

## The runbook

1. **Land all the work on `main` first.** Everything that should be in the release must be committed
   and pushed before any tag — the tag freezes the contents and the changelog range. Confirm
   `git status` is clean and `git rev-parse HEAD` equals `git rev-parse origin/main`.
2. **Confirm the latest `main` CI is green** (`gh run list --limit 5`): both `CI` and `cachix` should
   be `success` on the commit you are about to tag. The tag re-runs cachix from the same commit, so a
   red cachix on `main` will be red on the tag too.
3. **Pre-flight gates** (run them — do not assume; this is a public repo and the pipeline is rarely
   exercised):
   - `nix develop --accept-flake-config -c just test` — the leak guard lives here. **Never publish a
     tree leaking a real cluster/namespace/ARN name.** (`just check` is the gofmt+vet gate;
     golangci-lint there is advisory `|| true` and does not block.)
   - `nix run nixpkgs#goreleaser -- check` — validates `.goreleaser.yaml`.
   - **Dry-run the real pipeline against a LOCAL tag** — this is the highest-value gate and the one a
     snapshot can't give you (snapshot skips the changelog). A local tag is fully reversible; only the
     *push* is not:
     ```bash
     git tag vX.Y.Z                         # local only — NOT pushed
     rm -rf dist
     GITHUB_TOKEN="$(gh auth token)" nix run nixpkgs#goreleaser -- release --skip=publish --clean
     # inspect dist/CHANGELOG.md, and an extracted binary's `ksync version` (= X.Y.Z)
     git tag -d vX.Y.Z                      # remove the local tag before the real push
     ```
     If the tree is dirty with a yet-to-commit release change (e.g. a `.goreleaser.yaml` edit), add
     `--skip=publish,validate` to bypass the clean-tree check for the dry-run only.
4. **Push `main`** so the tagged commit exists on the remote: `git push origin main`.
5. **Create the lightweight tag on HEAD and push it** — *this is the public, hard-to-undo step.*
   Treat it like any outward-facing publish: do it only when the user has asked for the release (they
   did if they're invoking this skill); otherwise confirm first.
   ```bash
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```
6. **Verify BOTH workflows ran green:**
   ```bash
   gh run list --limit 5          # both "Release" and "cachix" should appear, triggered by the tag
   gh run watch <run-id>          # watch each to success
   gh release view vX.Y.Z         # the GitHub Release exists with the 4 archives + checksums
   ```
   Then prove **both install paths** resolve at the tag:
   ```bash
   # binary path: download an archive from the release and run `ksync version` → X.Y.Z
   nix run github:motoki317/ksync/vX.Y.Z -- version   # nix path → prints the short-rev (see quirk)
   ```

## Traps that leave a release half-published

- **Verifying only the GitHub Release.** The cachix job is a second workflow on the same tag; a green
  release with a red cachix means every downstream `nix run` compiles from source. Check both.
- **Tagging before pushing the commit.** The tag references a commit; if `main` wasn't pushed, the
  workflow runs against a commit the remote first saw via the tag push. Push `main` first.
- **Re-tagging a published version.** Don't move a `vX.Y.Z` tag after the release/cache artifacts
  exist — cut `vX.Y.(Z+1)` instead. Deleting or force-moving a remote tag is an outward-facing action
  with downstream pull impact; never do it without explicit user direction.
- **A `docs`/`chore`-only release shows an empty changelog.** Expected — those prefixes are filtered
  (scope-aware). If the release has user-facing changes, they must have landed as `feat`/`fix`/`perf`,
  not buried under an excluded prefix.
- **Assuming the first-release changelog will fail.** It won't: GoReleaser detects "no previous tag"
  and uses `git` instead of `github`. That log line is normal.

## Updating this skill

If the pipeline changes (a new workflow, a version moves into a file, the changelog config changes),
update this runbook AND the AGENTS.md "Releases" section together — they are the two operational
copies and must not drift.
