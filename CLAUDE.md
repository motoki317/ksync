# CLAUDE.md

Claude Code auto-loads this file. The full agent guide for this repo is **`AGENTS.md`** (the
cross-tool standard, also read by other agents); it is imported below so there is one source of
truth. Keep durable project rules in `AGENTS.md`, not here.

@AGENTS.md

## Local-only handoff

`HANDOFF.md` (gitignored; absent on a fresh clone) holds the full research context behind this
project: the tool-landscape survey that justified building ksync, the source-verified ArgoCD
semantic contract, the architecture sketch, and the milestone plan with acceptance criteria.
When it exists, **read it before design work** and do not re-litigate tool selection without it.
It is untracked because it references private environment details — never commit it or copy its
private references into tracked files (see the leakage rule in AGENTS.md).
