[private]
default:
    @just --list

# Build the ksync binary into ./ksync.
build:
    CGO_ENABLED=0 go build -o ksync ./cmd/ksync

# Run all tests (includes internal/leakcheck, the no-leak guard).
test:
    go test ./...

# Run all tests under the race detector. Kept separate from `test`/`pre-commit`
# (it needs cgo and runs ~2-3x slower); CI runs it on every push/PR.
test-race:
    CGO_ENABLED=1 go test -race ./...

# Static checks: gofmt gate + go vet + (advisory) golangci-lint.
check:
    @u="$(gofmt -l cmd internal)"; if [ -n "$u" ]; then echo "gofmt needed:"; echo "$u"; exit 1; fi
    go vet ./...
    golangci-lint run ./... || true

# Scan the repo for machine-local identifiers (see AGENTS.md, leakage rule).
leakcheck:
    go test ./internal/leakcheck/

# Pre-commit gate (wired as a git pre-commit hook by the flake's git-hooks
# integration): every commit must build; gofmt/vet and the tests (incl. the leak
# guard) cover the rest. (Skips the slow advisory golangci-lint that `check`
# runs, to keep per-slice commits fast.)
pre-commit: build
    @u="$(gofmt -l cmd internal)"; if [ -n "$u" ]; then echo "gofmt needed:"; echo "$u"; exit 1; fi
    go vet ./...
    go test ./...

# Verify the Nix flake build. Wired as a git pre-commit hook that runs ONLY when a
# commit touches dependency/flake files (see the `files` filter in flake.nix) — the
# only changes that can break the Nix path (a stale vendorHash or a flake error).
# That file gate is only sound because the flake sets proxyVendor, which pins
# vendorHash to go.mod/go.sum alone (see the comment in flake.nix).
# Slow (no cross-commit cache: the flake source hash changes every commit), so it
# is kept off the per-commit hot path.
nix-build:
    nix build .#ksync --no-link --print-build-logs

# Format Go sources.
fmt:
    go fmt ./...
