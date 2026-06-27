# Sync-failure diagnostic fixtures

Manifests that each induce one distinct "stuck / unhealthy" failure mode, for
eyeballing the diagnostic block `ksync sync` prints when an app does not become
healthy before its `--timeout` (`engine.Diagnose`, ADR
20260627-actionable-diagnostics). They need a live local cluster, so they are
**not** run by `go test` — the pure formatter and dedup/filter logic are
unit-tested in [`internal/engine/diagnose_test.go`](../../internal/engine/diagnose_test.go).
These fixtures are the manual counterpart: regenerate them after touching the
diagnostics to confirm the real output stays actionable.

## Run

`ksync.yaml` targets the `docker-desktop` context; edit `allowedContexts` for a
different local cluster. A short timeout makes each app give up quickly:

```bash
ksync build   # or use a prebuilt ./ksync
./ksync sync crashloop -f testdata/diagnostics/ksync.yaml -timeout 30s
```

Run a single scenario by name, or several at once. Clean up afterwards:

```bash
./ksync destroy crashloop -f testdata/diagnostics/ksync.yaml -yes
# or: kubectl delete ns -l '' $(kubectl get ns -o name | grep diag-)
```

## Scenarios

Each exercises a different path through the formatter; the right-hand column is
the actionable signal the dump must surface.

| Scenario | Failure mode | Diagnostic signal |
|----------|--------------|-------------------|
| `crashloop` | container exits 1 immediately, 3 replicas | `Waiting (CrashLoopBackOff); last: Terminated (Error, exit 1)`, the FATAL log, **3 replicas collapsed to one** `(+2 more identical)` |
| `badimage` | unresolvable image | `Waiting (ImagePullBackOff)` + the single most-informative pull error (`no such host`), the three redundant `Failed` events deduped to one |
| `unschedulable` | impossible memory request | pod has no container state — the `FailedScheduling` event is the sole signal |
| `badconfig` | `envFrom` a missing secret | `Waiting (CreateContainerConfigError)` + `secret "missing-secret" not found` |
| `readiness` | readiness probe to a closed port | `Running (not ready)` + `Unhealthy: Readiness probe failed` |
| `oom` | allocate past a 32Mi limit | exit 137 surfaced via last-termination (a real containerd/k3s reports `OOMKilled`; docker-desktop reports `Error, exit 137`) |
| `jobfail` | a `Job` that exits 2 | `Job ... reached the specified backoff limit` + the pod's `Terminated (Error, exit 2)` and migration error log |
| `initfail` | a failing init container | `init migrate: Waiting (CrashLoopBackOff); last: ...` + the **init container's** log, not the blocked app's empty one |
| `multicontainer` | one of two containers fails | both container states shown, the failing `sidecar`'s log tailed (not the healthy `web`) |
| `quota` | a `ResourceQuota` blocks pod creation | no pod exists, so the signal is the **ReplicaSet's** `FailedCreate: exceeded quota` — the "no child pod" class, gathered from the intermediate controller |

## What "actionable, not bloated" means here

- Normal events (`Scheduled`, `Pulled`, `Started`) and the redundant `BackOff`
  event are dropped — the container state and logs already carry the story.
- Identical replica failures collapse to one representative with a count.
- One log stream per pod (the previous run for a crashed container, else the
  current), with the kubelet "unable to retrieve container logs" placeholder
  filtered out.
- Child resources are walked: pods carry the usual signal, but an intermediate
  controller (a ReplicaSet) is shown when it holds a warning the pods cannot — a
  `FailedCreate` when no pod could be created at all (see `quota`). A controller
  whose pods came up fine emits only Normal events and stays out of the dump.
- A Ctrl-C of a one-shot `sync` is reported as `interrupted` (exit 130) but still
  prints the same dump for each app that was stuck — aborting a wedged sync is the
  usual way the failure is seen — gathered on a fresh context, never the cancelled
  one (which would read back only `context canceled`). A real timeout dumps the same
  way. `watch` does not dump on Ctrl-C: there it is a routine quit.
