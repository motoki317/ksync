---
name: release
description: >-
  Use when cutting a ksync release: publishing a new version of the ksync CLI, or when the user
  says "release X.Y.Z", "tag a release", "ship a new version", or "cut a release". Pushing a
  `vX.Y.Z` git tag fires both GoReleaser (GitHub Release) and the cachix pre-build. This skill is
  the runbook: the pre-flight gates, the exact order, and the traps that leave a release
  half-published. Use it whenever a git tag is about to trigger a public artifact.
---

# Cutting a ksync release

Pushing a `vX.Y.Z` tag starts two workflows, and both must pass.

| Workflow | Publishes |
| --- | --- |
| `.github/workflows/release.yaml` (GoReleaser, `.goreleaser.yaml`) | A GitHub Release: linux/darwin × amd64/arm64 tarballs (binary, LICENSE, README), `checksums.txt`, and the changelog |
| `.github/workflows/cachix.yaml` | `nix build .#ksync` on x86_64-linux, aarch64-linux, and aarch64-darwin, pushed to the `motoki317-ksync` cachix cache, so that `nix run github:motoki317/ksync` downloads the binary |

## Facts that shape the flow

- **The tag is the version.** There is no version file to bump. `main.version` in
  `cmd/ksync/main.go` defaults to `dev`. GoReleaser sets it from the tag
  (`-X main.version={{ .Version }}`), and the flake sets it from `self.shortRev`.
- **The two builds print different versions.** The GoReleaser binary's `ksync version` prints
  `X.Y.Z`. `nix run github:motoki317/ksync/vX.Y.Z -- version` prints the git short revision. This
  is expected. Do not fix it during a release.

## Runbook

1. **Put every change on `main`, and push it.** The tag freezes the contents and the changelog
   range. `git status` must be clean, and `git rev-parse HEAD` must equal
   `git rev-parse origin/main`.
2. **Check that CI passed on that commit.** Run `gh run list --commit "$(git rev-parse HEAD)"`.
   Both `CI` and `cachix` must show `success`. The tag runs cachix again on the same commit, so a
   failed cachix on `main` also fails on the tag.
3. **Preview the changelog and pick the version.** GoReleaser cannot dry-run the changelog. It
   asks GitHub to compare the previous tag with the new one, and that fails until the new tag is
   on GitHub. This command lists every subject that the notes will show:
   ```bash
   git log --pretty='%s' "$(git describe --tags --abbrev=0)"..HEAD \
     | grep -Ev '^(docs|test|chore|ci)(\(.+\))?!?:'
   ```
   The notes group `feat`, `fix`, and `perf` subjects under their own headings. Everything else,
   merge commits included, goes under Other. If the user gave no version, propose one from this
   list and ask.
4. **Run every pre-flight gate.** The repo is public and the pipeline runs rarely, so do not skip
   one.
   - `nix develop --accept-flake-config -c just test` runs the tests. CI passing is not enough:
     the leak guard skips in CI, so only this local run catches a leaked cluster, namespace, or
     ARN name. Never publish a tree that leaks one.
   - `nix run nixpkgs#goreleaser -- check` catches errors in `.goreleaser.yaml`.
   - Dry-run the build and the archives against a local tag:
     ```bash
     git tag vX.Y.Z                                  # local only, NOT pushed
     nix run nixpkgs#goreleaser -- build --clean     # all 4 targets, version from the tag
     dist/ksync_<os>_<arch>*/ksync version           # your host's binary prints X.Y.Z
     nix run nixpkgs#goreleaser -- release --snapshot --clean   # archives and checksums
     git tag -d vX.Y.Z                               # delete the local tag
     ```
     `--snapshot` skips the changelog and publishing. If a step fails, still delete the local
     tag, or the `git tag` in the next step fails.
5. **Create the tag on HEAD and push it.** This step is public and hard to undo. If the user did
   not ask for the release, ask before you push.
   ```bash
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```
6. **Check that both workflows passed.**
   ```bash
   gh run list --commit "$(git rev-parse HEAD)"   # "Release" and "cachix" run for the tag
   gh run watch <run-id>                          # watch each one until it succeeds
   gh release view vX.Y.Z                         # the 4 archives and checksums.txt exist
   ```
   Download an archive from the release and run `ksync version`. It must print `X.Y.Z`.
7. **Check that the cache holds the binary.** A green cachix run does not prove this. Without
   the `CACHIX_AUTH_TOKEN` secret, the workflow builds but cannot push, and `nix run` then still
   works by compiling from source. Query the cache for each system:
   ```bash
   for sys in x86_64-linux aarch64-linux aarch64-darwin; do
     p=$(nix eval --raw "github:motoki317/ksync/vX.Y.Z#packages.$sys.ksync.outPath")
     nix path-info --store https://motoki317-ksync.cachix.org "$p"   # "is not valid" = missing
   done
   ```

## Traps that leave a release half-published

- **Checking only the GitHub Release.** A failed cachix push does not show there. Then every
  `nix run` and downstream `nix develop` compiles ksync from source. Do steps 6 and 7.
- **Moving a published tag.** After the release or cache artifacts exist, cut `vX.Y.(Z+1)`
  instead. Deleting or moving a remote tag needs the user's explicit direction.

If the pipeline changes (a new workflow, a version file, a new changelog config), update this
skill. AGENTS.md only points here.
