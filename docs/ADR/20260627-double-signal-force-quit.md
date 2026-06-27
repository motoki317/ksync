---
date: "2026-06-27"
author: "motoki317"
status: "accepted"
---

# Context

Every command registered its interrupt handling with `signal.NotifyContext` at
the top of its `run` function, then proceeded through steps that are not all
context-aware. `engine.New` calls gitops-engine's `Run` → `EnsureSynced`, the
warm-cache full-cluster LIST, which ignores the passed context and takes seconds
on a large cluster (more so over a slow API hop, e.g. a microVM's k3s). Render
(kustomize + helm inflation) is likewise synchronous and not context-aware.

`signal.NotifyContext` disables Go's default-terminate the moment it registers.
So a SIGINT arriving during one of those blocking steps cancels the context the
step never consults — and, because `NotifyContext` only cancels once and then
keeps catching signals without acting, every later SIGINT is swallowed too. The
user cannot force-quit until the blocking step returns on its own. The symptom is
intermittent by nature: it bites only when Ctrl-C lands inside a non-context-aware
window, so a warm cache (sub-second `EnsureSynced`) almost never reproduces it
while a cold or large cluster reliably does.

A blackholed apiserver makes the mechanism deterministic: `EnsureSynced` blocks
on the TCP connect, a single Ctrl-C is swallowed for the full ~75s connect
timeout, and the process is unkillable from the keyboard until then.

# Decision

Replace `signal.NotifyContext` at all four entry points (sync, watch, destroy,
images) with a shared `signalContext` helper: the first SIGINT/SIGTERM cancels
the context (graceful, unchanged behavior), and the second `os.Exit(130)`s the
process unconditionally. A force-quit is therefore always one extra Ctrl-C away,
regardless of which step is blocking. The first signal also prints a one-line
hint so the user knows a second press will force-quit.

The helper's returned cancel doubles as the watch confirmation picker's quit hook
(in raw mode the terminal delivers no SIGINT, so the picker calls cancel itself),
which collapses watch's former two-layer signal/picker context into one.

# Consequences

The graceful path is unchanged: a single Ctrl-C during the context-aware
health-gate wait still exits in ~0.05s. The new guarantee is that the user is
never wedged — the second Ctrl-C exits even mid-`EnsureSynced`. Exit code 130 is
the conventional SIGINT status, so shells and scripts read the force-quit as an
interrupt.

# Impact

`os.Exit(130)` on the second signal skips deferred cleanup (`eng.Close`, the
footer stop, a picker's terminal restore). This is acceptable: the force-quit
windows are all in cooked mode (the picker owns its own raw-mode Ctrl-C and
restores the terminal on its own exit path), so no terminal state is left broken;
`eng.Close` only stops cache watches, which the OS reclaims on exit anyway. The
only residual edge is an external `kill -INT` twice while the picker is mid-draw,
which is not reachable from the keyboard.

# Alternatives

- **Make `EnsureSynced` context-aware so the first Ctrl-C aborts the warm.**
  gitops-engine's `Run`/`EnsureSynced` takes no context and abandoning it
  mid-warm would leak the cache goroutines. The double-signal handler covers
  every non-context-aware window uniformly without touching the engine.
- **Re-raise the default handler on the second signal** (`signal.Reset` +
  re-kill self) instead of `os.Exit`. It also skips Go defers, so it leaves no
  terminal state better off, and yields a less predictable exit status than a
  plain 130.
- **Keep `NotifyContext` and rely on the per-app `--timeout`.** The timeout
  bounds the health-gate wait, not the cache warm or render, so it does not make
  those windows interruptible.
