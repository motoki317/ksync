package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// This file holds ksync's help prose: the root description, each command's Long
// text and Example block, and the concept-topic pages reachable via
// `ksync help <topic>`. Keeping the copy here (not scattered across the command
// constructors in cli.go) makes the whole CLI manual reviewable in one place —
// the help IS the user guide, so docs/usage.md is only a pointer to it.

const rootLong = `ksync — a local-development sync loop for Kubernetes.

It watches local kustomize directories and, on change, renders, diffs, and applies the
affected app to a local cluster with ArgoCD-parity sync semantics: helm hooks, sync
waves, prune, server-side apply, and health gating. An app that builds from source is
rebuilt, tagged by content, and rolled in the same loop.

A minimal ksync.yaml:

  allowedContexts: [docker-desktop]    # the only clusters ksync may touch
  apps:
    - path: apps/web                   # a kustomize dir; name defaults to "web"
    - path: apps/api
      needs: [db]                      # applied only after db is Healthy
    - name: db
      path: apps/postgres

Safety: ksync acts only on a context listed in allowedContexts and never reads your
kubeconfig current-context. It prunes only resources it applied (labeled ksync.dev/app)
and never deletes a namespace.

Requirements: a local cluster and its kubectl context; helm on PATH only when a
kustomization inflates helmCharts (kustomize is built in); docker only when an app
builds from source.`

// Per-command Long text and Examples. Each is a self-contained page: what the
// command does, its sharp edges, and worked invocations — the detail the flag
// list alone cannot carry. Concept material common to several commands lives in
// the help topics below, linked by name.

const watchLong = `Run the main loop: build and sync every selected app once to converge the cluster, then
re-render, diff, and apply each app whose files change. With no app names, every app is
watched.

On an interactive terminal a prompt gates each rebuild after the first convergence:
Rebuild-all is the default (Enter), Space narrows to a subset, and an empty selection
skips. --auto, or a non-terminal, rebuilds automatically. Builds run ahead of needs
order, and a deploy never applies an image that has not finished building.

watch rebuilds images from source, so it rejects image overrides: the --image flag is not
offered, and a set KSYNC_IMAGE_OVERRIDES fails fast — use 'ksync sync' to deploy a
pre-built image.

See 'ksync help builds' and 'ksync help strategy'.`

const watchExample = `  ksync watch                # watch and sync every app
  ksync watch web api        # only these two apps
  ksync watch --auto         # no prompt; rebuild on every change`

const syncLong = `Build, render, and apply each selected app once, then exit. With no app names, every app
is synced in needs order. Apps run concurrently within the needs DAG; an app finishes
only when its resources are Healthy (up to --timeout).

On timeout — or on Ctrl-C of a wedged sync — ksync names the still-unhealthy resources
and dumps their events and pod logs, so a stuck rollout is diagnosable without a separate
kubectl session.

--image (or KSYNC_IMAGE_OVERRIDES) deploys a pre-built image instead of building it.

See 'ksync help strategy' for the render/diff/apply behavior and 'ksync help hooks' for
hook and sync-wave ordering.`

const syncExample = `  ksync sync                                  # sync every app once
  ksync sync web                              # just one app
  ksync sync --force                          # re-run hooks even with no manifest change
  ksync sync --image ghcr.io/app=web:pr-42    # deploy a pre-built image, skip its build`

const diffLong = `Render each selected app exactly as sync would — post-render patches, image overrides,
and live build-tag carry-forward — and print a per-resource unified YAML diff against
live cluster state: what a sync would create, update, or prune. Read-only: no build, no
apply. Secrets are masked.

By default the diff is a server-side dry-run apply, so a field the apiserver defaults or
prunes is not shown as drift; --client-diff uses the faster in-process diff instead.

See 'ksync help strategy'.`

const diffExample = `  ksync diff             # preview changes for every app
  ksync diff web api     # just these apps`

const renderLong = `Print each selected app's rendered manifests (kustomize + helm) to stdout. Read-only and
no docker, so images ksync builds locally are absent.

By default it renders against the cluster so helm 'lookup' resolves and capabilities
match; --offline-render uses plain 'helm template' (no cluster; lookup returns empty) and
byte-matches 'kustomize build --enable-helm --load-restrictor LoadRestrictionsNone'.

Manifests go to stdout and status to stderr, so 'ksync render web > web.yaml' is clean.`

const renderExample = `  ksync render web > web.yaml
  ksync render --offline-render web    # no cluster needed`

const imagesLong = `Print the canonical, containerd-normalized references of the images the selected apps
deploy — one per line, sorted and deduplicated — the exact set a cache or pre-pull tool
should scope to. Images ksync builds locally (any build: entry) are excluded: they are
local-only dev tags that are never pulled.

--live also reads the images of running pods in the apps' namespaces, capturing
operator-derived images that the manifests never name (an ECK Elasticsearch data image
from spec.version).`

const imagesExample = `  ksync images                 # images every app deploys
  ksync images --live | sort   # include operator-derived running-pod images`

const destroyLong = `Delete every resource ksync tracks (labeled ksync.dev/app) for the selected apps, in
reverse needs order. It prints its scope and the target context first and requires --yes
to proceed. Namespaces are never deleted.

With no app names, destroy targets every app — so a bare 'ksync destroy --yes' tears down
the whole stack.`

const destroyExample = `  ksync destroy web --yes    # delete one app's resources
  ksync destroy --yes        # tear down every app`

// Concept topics. Each is a help-only command (no Run): 'ksync help <topic>' or
// 'ksync <topic>' prints its Long. They carry the conceptual material that used
// to live in docs/usage.md — the ksync.yaml schema and the behavior no per-flag
// description can express.

const configTopic = `ksync reads one ksync.yaml (default: the working directory; -f <path> overrides).

  allowedContexts:
    - docker-desktop
    # - k3s-*                   # glob: matches a family of contexts

  apps:
    - path: apps/web            # name defaults to the directory name ("web")
    - name: api
      path: apps/api
      namespace: team-a
      needs: [db]
      build:
        - { image: ghcr.io/app/api, context: ../src/api }
    - name: db
      path: apps/postgres
      namespace: team-a

Top-level fields:
  allowedContexts   kubectl contexts ksync may target (>=1, required). The safety gate.
  apps              the app list (required).
  imageLoad         command that loads built images into a separate-store cluster.
  buildGroups       named bulk-build commands shared by build entries.

App fields:
  path          directory with a kustomization file, relative to the config (required).
  name          app name (default: the directory name); used in commands, logs, labels.
  namespace     default namespace for resources that declare none.
  needs         apps that must sync and be Healthy before this one. Cycles are rejected.
  build         images built from local source (see 'ksync help builds').
  patches       post-render edits to one rendered object (below).
  clientRender  render this app client-side so it can bundle its own CRDs and CRs
                (see 'ksync help strategy').

Unknown fields are rejected.

Context selection
  allowedContexts is the safety gate: ksync targets only a listed context and never reads
  the kubeconfig current-context. A sole concrete entry is auto-targeted; with several
  entries or a glob you must pass --context <name>, and it must match an entry. Entries
  are shell globs — k3s-* matches a family of per-worktree clusters — and a --context
  matched by glob (not an exact entry) warns, since a glob is a broader grant. '*' matches
  everything and defeats the gate.

Default namespace
  namespace sets the default for rendered resources that declare none (like ArgoCD's
  destination.namespace); applying a namespaceless resource without it fails. Resources
  with their own metadata.namespace keep it. ksync creates the namespace if missing; no
  namespace is ever labeled, pruned, or deleted.

Per-environment patches
  patches edits a rendered field the kustomization cannot express per environment — a
  hostPath whose value depends on where ksync runs. The edit applies after render, before
  image injection.

    patches:
      - target: { kind: Deployment, name: cache }   # exact kind+name(+group/version/ns)
        patch: |                                    # must match exactly one object
          - op: replace
            path: /spec/template/spec/volumes/0/hostPath/path
            value: ${HOME}/.cache/web

  patch is an inline RFC 6902 op list; lead with a test op on a stable field, since it
  indexes arrays. ${VAR} in a value expands at render time (the environment plus
  ${KSYNC_WORKDIR}, the config dir): ${VAR:-default} falls back when unset or empty, a
  bare undefined ${VAR} fails the render, and $$ is a literal $. ${HOME} is the cluster
  host's home, since ksync runs next to it.`

const buildsTopic = `A build entry rebuilds an image from local source on change, content-tags it, injects the
tag, and rolls the pods.

  build:
    - image: ghcr.io/app/api    # as the manifests reference it, without a tag
      context: ../src/api       # docker build context; watched for changes

Build fields:
  image         image name as manifests reference it (no tag/digest); ksync replaces the
                tag of every match. Required.
  context       build context directory, relative to the config; watched. Required.
  name          label in progress and prompt output. Default: the image's last segment.
  dockerfile    Dockerfile relative to context. Default: Dockerfile.
  watch         paths under context that trigger a rebuild. Default: the whole context.
  watchIgnore   .dockerignore-syntax paths kept in the context but excluded from
                triggering a rebuild.
  command       replaces docker build (must leave the image under $KSYNC_IMAGE).
  group         builds via a top-level buildGroups entry (excludes command/dockerfile).

One image name has at most one build definition across all apps.

Key behavior
  - The tag is 'ksync-' plus a hash of the image's layers and config, so identical content
    keeps the same tag (no pod restart) even when the build tool restamps the image ID. No
    build state is persisted; startup rebuilds once via docker's layer cache.
  - An explicit imagePullPolicy: Always on a built image is rewritten to IfNotPresent —
    the local tag exists in no registry, so Always would force a failing pull.
    Manifest-only edits skip docker.
  - .dockerignore-excluded files are not watched. A staged build output the Dockerfile
    COPYs but that must not retrigger the build goes in watchIgnore.

Custom and grouped builds
  command replaces docker build for one image; a top-level buildGroups entry builds
  several with one command (a multi-target 'docker buildx bake', or a host compile
  producing many binaries). Members reference the group by name; a one-shot sync builds
  every selected entry, while an incremental watch change rebuilds only the dirty members.
  Both run via 'sh -c' in the context and receive $KSYNC_IMAGES (the refs to produce,
  newline-separated), $KSYNC_IMAGE (the first), and $KSYNC_CONTEXT.

    buildGroups:
      - name: services
        command: |
          for ref in $KSYNC_IMAGES; do
            t=${ref%:*}; t=${t##*/}; sets="$sets --set ${t}.tags=${ref}"; targets="$targets $t"
          done
          docker buildx bake --load $sets $targets
    apps:
      - name: services
        path: manifests/services
        build:
          - { image: ghcr.io/app/api, context: ../.., watch: [services/api, lib], group: services }
          - { image: ghcr.io/app/web, context: ../.., watch: [services/web, lib], group: services }

  A grouped command must leave each requested image tagged <image>:ksync-build; ksync
  content-tags each afterward. Double-quote "$KSYNC_IMAGE" (single quotes do not expand
  under sh -c). When two apps share a group, ksync runs that group's command for one app
  at a time by default (a bulk command need not be safe run against itself); set
  parallel: true on the buildGroups entry when it is concurrency-safe.

Pre-built image overrides
  'ksync sync --image IMAGE=REF' (or KSYNC_IMAGE_OVERRIDES, also on diff) deploys an
  existing image instead of building it — a CI artifact, registry tag, or pinned digest.
  REF may be a bare tag, name:tag, or @digest. The build and its imageLoad are skipped, so
  making the ref cluster-visible is the supplier's job; an override for an unbuilt image
  is ignored. watch rejects overrides.

Making built images visible (imageLoad)
  ksync builds into the local docker daemon. Docker Desktop runs pods from it directly;
  k3d, kind, and remote clusters keep a separate image store, so a built tag must be
  loaded. imageLoad.command runs (with $KSYNC_IMAGES, $KSYNC_IMAGE, $KSYNC_CONTEXT) only
  when an image is rebuilt; calls are serialized and coalesced.

    imageLoad:
      command: k3d image import --cluster dev $KSYNC_IMAGES
      # kind:     kind load docker-image --name dev $KSYNC_IMAGES
      # k3s:      docker save $KSYNC_IMAGES | k3s ctr -n k8s.io images import -
      # registry: for i in $KSYNC_IMAGES; do docker push "$i"; done

  With k3d/kind, set pods to imagePullPolicy: Never or IfNotPresent. Branch on
  $KSYNC_CONTEXT to share one config between a shared-daemon and a separate-store cluster
  (exit 0 when there is nothing to load).`

const strategyTopic = `ksync runs three phases. Each defaults to the cluster-aware behavior and has an opt-out
for when that default is wrong for one app or one run.

  Phase    Default                                          Opt-out
  Render   helm template against the cluster (lookup works) clientRender: true (per app)
                                                            --offline-render (per run)
  Diff     server-side dry-run apply (no false drift)       --client-diff (per run)
  Apply    server-side apply; prune by the ksync.dev/app    --prune=false; --force
           label

Render — clientRender: true
  The default render is a server-side dry-run, so the apiserver maps every resource. An app
  whose chart ships a CRD together with custom resources of that kind then fails with 'no
  matches for kind' until the CRD exists. clientRender renders that app client-side —
  keeping the cluster's capabilities but dropping the dry-run, the way ArgoCD renders — so
  one app can own both its CRDs and its custom resources. It disables helm lookup for the
  app, so use it only where the chart does not need lookup.

Render — --offline-render
  Renders with no cluster at all (plain helm; lookup returns empty), byte-matching
  'kustomize build --enable-helm --load-restrictor LoadRestrictionsNone'. Accepted by
  render, sync, watch, diff, and images; it overrides clientRender (every app renders
  plain).

Diff — --client-diff
  The server-side dry-run keeps a field the apiserver defaults or prunes (a StatefulSet
  maxUnavailable behind a disabled feature gate) from showing as drift. --client-diff uses
  the faster in-process merge instead, at the cost of showing such a field as a perpetual
  change — harmless on sync/watch, visible drift on diff.

Tracking, prune, and safety
  Every applied non-Namespace resource gets the label ksync.dev/app: <app name>
  (server-side apply, field manager ksync). Prune (on sync/watch) and destroy act only on
  resources carrying it with the matching app name; everything else is invisible to them.
  - ksync targets only an allowedContexts entry and ignores the kubeconfig current-context.
  - Namespaces are never labeled, pruned, or deleted — even one an app renders.
  - An app that renders zero resources while it still owns live ones will not prune them
    away: ksync refuses and points at 'ksync destroy <app>' for an intentional teardown.
  - destroy requires --yes and prints its scope first.

Output
  Status goes to stderr; render manifests go to stdout. Color is on for an interactive
  terminal and off under NO_COLOR. On a terminal, each app is a live pipeline
  (Build -> Import -> Deploy) ending in a one-line summary; a pipe or CI streams each
  build's output line by line instead.`

const hooksTopic = `ksync follows ArgoCD's hook rules, not Helm's.

  - Hooks are read from argocd.argoproj.io/hook, falling back to helm.sh/hook (excluding
    crd-install).
  - post-install / post-upgrade become PostSync and run on every sync, after the main
    resources are Healthy.
  - helm.sh/hook-weight is the sync wave when argocd.argoproj.io/sync-wave is absent.
  - helm.sh/hook-delete-policy maps as in ArgoCD (default BeforeHookCreation).

A no-change sync skips hooks, unless a hook's Job is currently failed (it re-runs so a
transiently-failed PostSync self-heals) or --force re-runs them all (ArgoCD manual-sync
parity).`

const troubleshootingTopic = `Most errors name their own fix; the recurring ones:

  <app> rendered 0 resources but manages N live resource(s)
    The kustomization now renders nothing (a typo, a dropped resources: entry). Fix it, or
    'ksync destroy <app>' to remove the app.

  cluster cannot map X (Y)
    Live render asks the cluster to map every kind; a CRD not yet installed or an unserved
    apiVersion fails. Install the CRD; if the app bundles its own CRDs with custom
    resources of that kind, set clientRender: true on it; or use --offline-render (helm
    lookup results will be empty).

  Pods of a built image show ErrImagePull
    The kubelet tried to pull the local ksync-... tag. The image has no matching build
    entry, the cluster has a separate store with no imageLoad, or it cannot see the docker
    daemon.

  Pods restart on every rebuild with no source change
    The image ID changes each build (usually a provenance attestation). ksync's docker
    build disables it; with a command, add --provenance=false.

  A change is not picked up by watch
    ksync watches the app directory and what the kustomization references, minus
    .dockerignore and watch: exclusions. For anything outside, run 'ksync sync'.

  First sync after start is slow
    ksync lists the cluster once to warm its cache. Keep watch running; later syncs reuse
    it.`

// helpTopic is one concept page. Running 'ksync <name>' prints its guide, and
// 'ksync help <name>' shows the same text through cobra's help. It carries a Run
// (not merely a Long) so cobra lists it under the topics group rather than its
// catch-all "Additional help topics" section.
func helpTopic(name, short, long string) *cobra.Command {
	return &cobra.Command{
		Use:     name,
		Short:   short,
		Long:    long,
		GroupID: groupTopics,
		// A topic takes no args; reject stray words so a typo like 'ksync config
		// apps' is an error, not silently ignored.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), long)
			return err
		},
	}
}

// helpTopics returns the concept pages, in the order the root help lists them.
func helpTopics() []*cobra.Command {
	return []*cobra.Command{
		helpTopic("config", "ksync.yaml schema: apps, contexts, namespaces, patches", configTopic),
		helpTopic("builds", "building images from source: build, buildGroups, imageLoad, overrides", buildsTopic),
		helpTopic("strategy", "how render, diff, apply, prune, and safety behave", strategyTopic),
		helpTopic("hooks", "ArgoCD hook mapping and sync-wave ordering", hooksTopic),
		helpTopic("troubleshooting", "common errors and their fixes", troubleshootingTopic),
	}
}
