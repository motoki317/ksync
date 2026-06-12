---
date: "2026-06-12"
author: "motoki317"
status: "accepted"
---

# Context

ksync needs to know which local directories are apps, what to call each app, which cluster it
is allowed to touch, and in what order dependent apps must sync. These choices shape every
later layer (rendering, dirty-set mapping, scheduling, prune), so they are fixed first, before
the sync engine lands.

# Decision

A single `ksync.yaml` file declares everything; `internal/config` owns its model:

- **App = one kustomization directory.** This mirrors the ApplicationSet directory-generator
  model that production GitOps setups use, so a manifest repo maps 1:1 onto ksync apps.
- **Explicit app list, no auto-discovery.** Each entry: `path` (required, relative to the
  config file), `name` (defaults to the directory basename, again matching the directory
  generator), and optional `needs` (names of apps that must sync first).
- **App names must be valid Kubernetes label values** (validated with apimachinery), because
  the name becomes the value of the ksync tracking label that scopes prune.
- **`context` is required and is the only kubectl context ksync will ever use.** ksync never
  reads the ambient current-context. Naming the one allowed target in config *is* the safety
  model.
- **Strict parsing**: unknown YAML fields are errors (catches typos like `need:`), all
  validation errors are reported together, `needs` must form a DAG over declared app names.

# Consequences

- The config file is the single place where dependency edges and (M2) build config attach —
  the reason auto-discovery was rejected.
- Prune safety starts at config validation: a name that cannot be a label value is rejected
  before it can ever be stamped on a cluster resource.
- Pointing ksync at a new manifest repo is one small YAML file; defaults (name = basename)
  keep it close to a bare list of paths.

# Impact

- Every subcommand starts by loading `ksync.yaml` (default `./ksync.yaml`, `-f` to override).
- Changing the schema later means migrating user configs; additions are cheap (strict parsing
  only rejects *unknown* fields), renames are not.

# Alternatives

- **Auto-discovering `kustomization.yaml` directories** — rejected: per-app settings
  (`needs`, M2 build config) need a declaration to attach to, and an implicit app set makes
  "what will ksync touch" unanswerable without running it.
- **Tilt-style `allow_k8s_contexts` allowlist** — rejected: Tilt needs an allowlist because it
  follows the *ambient* current-context. ksync instead requires the target context to be named
  explicitly and never falls back, which is strictly safer with one less moving part. A
  multi-context allowlist can be added later without breaking the schema.
- **Per-app config files next to each kustomization** — rejected for M1: cross-app concerns
  (dependency edges, shared watch roots) have no natural home, and the manifest repo may not
  be writable by the ksync user.

# Notes

- `needs` is the field name (over `dependsOn`/`barriers`): reads naturally in YAML
  (`needs: [db]`) and stays short.
- File-existence checks (does the path contain a kustomization file?) happen at load time,
  not parse time, so the pure parser/validator stays unit-testable without a filesystem.
