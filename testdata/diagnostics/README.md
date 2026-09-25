# Sync-failure diagnostic fixtures

Each app here fails in one distinct way and never becomes Healthy. They show the diagnostic block
that `ksync sync` prints for a stuck app (ADR 20260627-actionable-diagnostics).
`go test` does not run them, because they need a live local cluster. The unit tests in
[`internal/engine/diagnose_test.go`](../../internal/engine/diagnose_test.go) cover the formatter,
the filters, and the dedup. After you change `engine.Diagnose` (`internal/engine/diagnose.go`) or
`cmd/ksync/diagnostics.go`, run these fixtures to check that the real output still says what to
fix.

## Run

`ksync.yaml` targets the `docker-desktop` context. For another local cluster, put its context in
`allowedContexts`, but do not commit that edit. The leak check scans the working tree and fails
every commit while the edit is there, so revert it after the run
(`git checkout -- testdata/diagnostics/ksync.yaml`). A short `--timeout`
makes each app give up quickly. Run from the repo root:

```bash
just build
./ksync sync crashloop -f testdata/diagnostics/ksync.yaml --timeout 30s
```

Name one scenario, several, or none to run all ten. Each scenario runs in the namespace
`diag-<scenario>`. `destroy` deletes the resources but leaves the namespace, so delete it too:

```bash
./ksync destroy crashloop -f testdata/diagnostics/ksync.yaml --yes
kubectl --context <context> delete namespace diag-crashloop   # the context in allowedContexts
```

## Scenarios

The right column is the signal that the dump must show.

| Scenario | Failure mode | Diagnostic signal |
|----------|--------------|-------------------|
| `crashloop` | container exits 1 at once, 3 replicas | `Waiting (CrashLoopBackOff); last: Terminated (Error, exit 1)`, the FATAL log line, and one pod for all 3 replicas: `(+2 more identical)` |
| `badimage` | image cannot resolve | `Waiting (ImagePullBackOff)` and one pull error (`no such host`), not three `Failed` events |
| `unschedulable` | impossible memory request | only the `FailedScheduling` event, because the pod has no container state |
| `badconfig` | `envFrom` a missing secret | `Waiting (CreateContainerConfigError)` and `secret "missing-secret" not found` |
| `readiness` | readiness probe to a closed port | `Running (not ready, restarts 0)` and `Unhealthy: Readiness probe failed` |
| `oom` | allocates past a 32Mi limit | exit 137 in the last termination: `OOMKilled` on a containerd cluster such as k3s, `Error, exit 137` on Docker Desktop |
| `jobfail` | a `Job` that exits 2 | `Job ... reached the specified backoff limit`, the pod's `Terminated (Error, exit 2)`, and the migration error log |
| `initfail` | an init container fails | `init migrate: Waiting (CrashLoopBackOff); last: ...` and the **init container's** log, not the blocked app container's empty log |
| `multicontainer` | one of two containers fails | both container states, and the log of the failing `sidecar`, not the working `web` |
| `quota` | a `ResourceQuota` blocks pod creation | no pod exists, so the signal is the **ReplicaSet's** `FailedCreate: exceeded quota` |

## What the dump keeps and drops

- It drops Normal events (`Scheduled`, `Pulled`, `Started`) and the redundant `BackOff` warning.
- Each pod gets one log stream: the previous run for a crashed container, otherwise the current
  one. The kubelet placeholder "unable to retrieve container logs" is filtered out.
- Ctrl-C on a one-shot `sync` exits with `interrupted` (exit 130). It still prints the dump for
  each stuck app, the same as a timeout does. The dump reads the cluster on a fresh context, so a
  dump that shows only `context canceled` is a bug. `watch` prints no dump on Ctrl-C, because
  there Ctrl-C is a normal quit.
