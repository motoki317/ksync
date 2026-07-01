# ksync user guide

The user guide is the CLI's own help — it is self-contained, so this file only points into it.

- `ksync help` — the overview, command list, and concept guides.
- `ksync <command> -h` — one command's behavior, flags, and examples (e.g. `ksync sync -h`).
- `ksync help <topic>` (or `ksync <topic>`) — the concept guides:
  - `config` — the ksync.yaml schema: apps, contexts, namespaces, patches.
  - `builds` — building images from source: `build`, `buildGroups`, `imageLoad`, overrides.
  - `strategy` — how render, diff, apply, prune, and safety behave.
  - `hooks` — ArgoCD hook mapping and sync-wave ordering.
  - `troubleshooting` — common errors and their fixes.

Install: see the [README](../README.md#install). Design rationale: the [ADRs](ADR/).
