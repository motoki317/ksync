# CLAUDE.md

Claude Code auto-loads this file. The full agent guide for this repo is **`AGENTS.md`** (the
cross-tool standard, also read by other agents); it is imported below so there is one source of
truth. Keep durable project rules in `AGENTS.md`, not here.

@AGENTS.md

## Research context (before design work)

The research behind ksync is tracked, not machine-local:

- **Why ksync exists / build-vs-buy** — [docs/ADR/20260612-build-vs-buy-tool-landscape.md](docs/ADR/20260612-build-vs-buy-tool-landscape.md).
  Read it before re-litigating tool selection; do so only with new evidence.
- **The verified ArgoCD semantic contract** — [docs/argocd-parity.md](docs/argocd-parity.md).

A gitignored `HANDOFF.md` may still exist on a given machine, holding only private
reference-environment details (specific cluster/repo names). It is optional local scratch — no
tracked work depends on it. Never commit it or copy its private references into tracked files
(see the leakage rule in AGENTS.md).
