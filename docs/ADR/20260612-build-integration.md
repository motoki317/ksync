# Native build integration

Date: 2026-06-12
Status: accepted

## Context

Milestone 2 closes the loop from *source* change, not just manifest change: edit a service's
source code and the local cluster runs the rebuilt image — the docker-compose experience,
through the same render→diff→apply pipeline that manifest edits already use.

The dominant design pressure is surface area. The tools that already do source-triggered
builds answer every environment variation with configuration: Skaffold's
`build`/`tagPolicy`/`artifacts` trees, Tilt's Starlark programs. ksync's niche is narrower —
local microservices, one local cluster, docker available — so most of those knobs can be
decisions instead of options. A first-time visitor should be able to read a `ksync.yaml`
with builds in it and understand the whole thing.

## Decision

1. **Config is a `build` list per app with two required fields.** `image` (the image name
   exactly as the rendered manifests reference it) and `context` (the build context
   directory). Three optional fields, each earning its place: `dockerfile` (monorepos keep
   Dockerfiles per service with a shared root context), `watch` (reaction paths, for
   monorepo contexts where only some paths feed a given image), and `command` (the single
   escape hatch replacing build-arg/target/platform/cache knobs: any shell command that
   leaves the image tagged `$KSYNC_IMAGE` in the local docker daemon; run via `sh -c` with
   the context as working directory). Tags, transport, and build state are ksync-managed
   and deliberately not configurable.

2. **Dev tags are content-addressed; there is no persisted build state.** After a build,
   ksync reads the image ID and tags it `<image>:ksync-<12 hex of the ID>`. Unique tags are
   what make Deployments roll out (a fixed dev tag would re-apply an unchanged pod spec);
   content-addressed unique tags additionally make rebuilds idempotent: unchanged source
   hits the docker layer cache, yields the same ID, the same tag, an unchanged spec — no
   rollout. That property replaces the image-manifest state file used by prior art
   (Skaffold's artifacts JSON and similar): ksync simply rebuilds on startup, and a restart
   converges instead of trusting a possibly stale file.

3. **Tag injection has kustomize `images:` semantics and happens in-process.** The built
   tag is applied to the rendered resmap (before sync objects are extracted) with the same
   filters kustomize's builtin images transformer uses, so the result is identical to the
   user adding `images: [{name, newTag}]` to their kustomization — no temp overlay, no
   worktree mutation. The deprecated `api/builtins` re-export is avoided; ksync composes
   the public `api/filters/imagetag` filters with kustomize's default field specs.

4. **Transport is the local docker daemon.** docker-desktop Kubernetes runs pods from
   daemon images directly, and a unique non-`latest` tag gets `imagePullPolicy:
   IfNotPresent` by Kubernetes default, so no registry and no load step are needed.
   Per-cluster image loaders (kind/k3d `image import`, k3s tar drop) are deferred until a
   real need; they slot in behind the build package without config changes.

5. **Builds are scheduled per (app, entry), never inline with every sync.** A source change
   dirties the build entry; the entry rebuilds at the start of that app's next run, before
   render. Manifest-only edits never invoke docker — the change→applied latency target
   (p50 ≤ 2s) survives having builds configured. A failed build re-dirties its entry, so
   the scheduler's existing backoff covers build retries; a successful build's tag is
   remembered in memory and reused until the source changes again.

6. **Watch scope honors `.dockerignore`.** A file docker would not copy into the build
   context cannot change the image, so ksync neither watches it nor lets it dirty a build.
   This is load-bearing, not cosmetic: it prevents self-triggered rebuild loops (`command`
   builds that write artifacts into the context, e.g. compile-on-host flows targeting a
   thin Dockerfile) and watcher exhaustion on big contexts (fsnotify on macOS uses kqueue —
   one descriptor per watched directory; a Rust `target/` tree alone can exceed the
   default limit). `<dockerfile>.dockerignore` takes precedence over
   `<context>/.dockerignore`, matching BuildKit.

7. **`ksync render` stays offline.** It prints manifests as written, without dev tags —
   rendering never talks to docker or the cluster. `ksync sync` and `ksync watch` build
   all entries up front so the manifests they apply always reference images that exist.

## Consequences

- Apps with `build` entries require the docker CLI (BuildKit). Apps without remain
  docker-free.
- A chart that hardcodes `imagePullPolicy: Always` cannot run locally built images (the
  kubelet would try to pull the ksync tag from the registry). Documented in
  troubleshooting; most charts parameterize the policy.
- Images exist only in the local daemon: multi-node clusters and clusters that cannot see
  the daemon (kind, remote) are out of scope until image loaders land.
- Startup with builds configured costs one cached rebuild per entry (typically around a
  second when nothing changed) — the price of having no state file.
- New dependency: `github.com/moby/patternmatcher`, the reference `.dockerignore`
  implementation (negation patterns included), rather than a hand-rolled matcher.
