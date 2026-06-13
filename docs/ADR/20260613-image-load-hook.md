# Image-load hook for clusters with a separate image store

Date: 2026-06-13
Status: accepted

## Context

The build-integration ADR (2026-06-12, decision 4) chose the local docker daemon as the only
image transport, because Docker Desktop's Kubernetes runs pods from daemon images directly. It
explicitly deferred "per-cluster image loaders (kind/k3d `image import`, k3s tar drop) until a
real need."

Dogfooding against a real k3d-based project surfaced that need. k3d (and kind) run their own
containerd image store inside the node containers; the local docker daemon's images are *not*
visible there. A content-addressed dev tag built by ksync (`<image>:ksync-<hash>`) exists only
in the docker daemon, so a pod referencing it fails to start — `ErrImageNeverPull` under the
`imagePullPolicy: Never` these setups use, or a registry pull attempt otherwise. The build loop
is correct end to end *except* that the freshly built image never reaches the cluster.

## Decision

Add one optional top-level config field, `imageLoad`: a shell command run once per freshly built
ref, via `sh -c`, with `$KSYNC_IMAGE` set to that full ref. It runs immediately after a
successful build, before the tag is injected and synced. Empty (the default) means the cluster
shares the docker daemon's images and nothing runs.

This reuses the exact contract the build `command` field already established (`sh -c` +
`$KSYNC_IMAGE`), so a reader who understands one understands the other. The field is top-level,
not per-build, because image visibility is a property of the *target cluster*, not of any one
image — every image loads the same way into the same cluster.

Examples:

```yaml
imageLoad: k3d image import --cluster dev $KSYNC_IMAGE
imageLoad: kind load docker-image --name dev $KSYNC_IMAGE
imageLoad: docker push $KSYNC_IMAGE        # remote cluster pulling from a registry
```

In watch mode a load failure is treated like a build failure: the entry re-dirties and the
scheduler retries it with backoff, so a transient import error does not wedge the loop.

## Consequences

- ksync now closes the source→applied loop on k3d/kind, not just daemon-shared clusters — the
  common local-dev cluster types. Verified live on k3d: build → `k3d image import` → server-side
  apply → pod `Running` on the locally built content-addressed image, and an incremental source
  edit rebuilds, re-imports, and rolls the pod.
- Zero cost for Docker Desktop users: the field is unset and no load step runs.
- The build package gains a `Loader` beside `Builder`; `cmd/ksync` composes build-then-load into
  the one `BuildFunc` both `sync` and `watch` consume, so there is a single code path.

## Impact

- ksync carries **no** per-cluster-type knowledge: it does not detect k3d/kind or shell out to
  their CLIs by name. That logic lives entirely in the user's `imageLoad` string. The cost is
  that the user must write one line; the benefit is that any present or future transport works
  without a ksync change.
- The load runs once per built ref. A tool like `k3d image import` spins up a helper node and
  transfers a tarball (~seconds) per invocation, so a cold start that builds many images pays
  that cost serially. The hot path — editing one component — rebuilds and imports just that one
  image, which is the case that matters for the inner loop. Batching many loads into one
  invocation was considered and rejected for now: it would couple the loop's per-entry build
  scheduling to a cross-entry load barrier for a one-time startup cost. Revisit only if
  many-image cold start becomes a real pain point.

## Alternatives

- **Auto-detect the cluster type** (e.g. derive `k3d image import --cluster X` from a
  `k3d-X` context name). Rejected: it bakes per-tool knowledge and naming conventions into ksync,
  breaks the moment a tool changes its CLI, and hides what is actually run. The generic command
  is smaller and strictly more capable; auto-detection could still be layered on later as a
  convenience default if the one-line config proves to be friction.
- **A built-in `loader: k3d|kind|push` enum.** Same objection as auto-detect with less
  flexibility — every new transport needs a code change and a new enum value.
- **Push to a registry unconditionally.** Forces registry setup on every user and changes the
  image ref the manifests must use; the daemon-shared fast path (Docker Desktop) would regress.

## Notes

The `command` build escape hatch and `imageLoad` are deliberately symmetric: build *produces*
`$KSYNC_IMAGE`, load *consumes* it. For monorepo bake flows (one Dockerfile, many image targets)
each image is one `build` entry whose `command` bakes that single target; `imageLoad` then makes
each built ref visible. Per-entry `watch` scopes keep an edit to one component rebuilding only
that component — a finer-grained loop than a "rebuild and re-import everything" script.
