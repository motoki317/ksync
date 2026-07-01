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

Watch local kustomize directories. On change, render, diff, and apply the affected app
to a local cluster the way ArgoCD does: hooks, sync waves, server-side apply, prune
(delete resources you removed from the manifests), and a wait until resources are
Healthy. An app that builds from source is rebuilt and redeployed in the same loop.

A minimal ksync.yaml:

  allowedContexts: [docker-desktop]    # kubectl contexts ksync may use
  apps:
    - path: apps/web                   # a kustomize dir; name defaults to "web"
    - path: apps/api
      needs: [db]                      # applied only after db is Healthy
    - name: db
      path: apps/postgres

First run:

  1. write a ksync.yaml here     list your apps and cluster
  2. ksync diff                  preview what a sync would change
  3. ksync sync                  apply it once
  4. ksync watch                 re-sync on every change

New to ksync? Read 'ksync help config' to write your ksync.yaml.`

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
offered, and setting KSYNC_IMAGE_OVERRIDES makes it fail fast — use 'ksync sync' to deploy
a pre-built image.

See 'ksync help builds' and 'ksync help strategy'.`

const watchExample = `  ksync watch                # watch and sync every app
  ksync watch web api        # only these two apps
  ksync watch --auto         # no prompt; rebuild on every change`

const syncLong = `Build, render, and apply each selected app once, then exit. With no app names, every app
is synced in needs order. Apps run concurrently, in the order set by needs; an app
finishes only when its resources are Healthy (up to --timeout).

On timeout — or if you Ctrl-C a stuck sync — ksync names the still-unhealthy resources
and dumps their events and pod logs, so a stuck rollout is diagnosable without a separate
kubectl session.

--image (or KSYNC_IMAGE_OVERRIDES) deploys a pre-built image instead of building it.

See 'ksync help strategy' for the render/diff/apply behavior and 'ksync help hooks' for
hook and sync-wave ordering.`

const syncExample = `  ksync sync                                  # sync every app once
  ksync sync web                              # just one app
  ksync sync --force                          # re-run hooks even with no manifest change
  ksync sync --image ghcr.io/app/api=pr-42    # deploy tag pr-42 of api instead of building`

const diffLong = `Render each selected app exactly as sync would, then print a per-resource unified YAML
diff against live cluster state: what a sync would create, update, or prune. Read-only:
no build, no apply. Secrets are masked. The render applies the same post-render patches,
image overrides, and build-tag carry-forward as a real sync.

By default the diff is a server-side dry-run apply, so a field the apiserver defaults or
prunes is not shown as drift; --client-diff uses the faster in-process diff instead.

See 'ksync help strategy'.`

const diffExample = `  ksync diff             # preview changes for every app
  ksync diff web api     # just these apps`

const renderLong = `Print each selected app's rendered manifests (kustomize + helm) to stdout. Read-only and
no docker, so a built image appears at its declared ref, not the locally-built dev tag.

By default it renders against the cluster so helm 'lookup' resolves and capabilities
match; --offline-render uses plain 'helm template' (no cluster; lookup returns empty) and
byte-matches 'kustomize build --enable-helm --load-restrictor LoadRestrictionsNone'.

Manifests go to stdout and status to stderr, so 'ksync render web > web.yaml' is clean.`

const renderExample = `  ksync render web > web.yaml
  ksync render --offline-render web    # no cluster needed`

const imagesLong = `Print the images the selected apps deploy — one per line, sorted and deduplicated — each
as a canonical (containerd-normalized) reference, so a cache or pre-pull tool matches the
cluster's image store by string equality. Images ksync builds locally (any build: entry)
are excluded: they are local-only dev tags that are never pulled.

--live also reads the images of running pods in the apps' namespaces, capturing
operator-derived images that the manifests never name (an ECK Elasticsearch data image
from spec.version).`

const imagesExample = `  ksync images          # images every app deploys
  ksync images --live   # also include operator-derived running-pod images`

const destroyLong = `Delete every resource ksync tracks (labeled ksync.dev/app) for the selected apps, in
reverse needs order. It prints its scope and the target context first and requires --yes
to proceed. Namespaces are never deleted.

With no app names, destroy targets every app — so a bare 'ksync destroy --yes' tears down
the whole stack.`

const destroyExample = `  ksync destroy web --yes    # delete one app's resources
  ksync destroy --yes        # tear down every app`

// Concept topics. Each is a help-only command: 'ksync <topic>' prints its Long and
// 'ksync help <topic>' shows the same text. They do no cluster work but carry a Run
// (see helpTopic). They hold the conceptual material that used to live in
// docs/usage.md — the ksync.yaml schema and the behavior no per-flag description can
// express.

const configTopic = `ksync reads one ksync.yaml from the working directory (use -f <path> for another).

## A complete example

  allowedContexts:
    - docker-desktop            # kubectl context names (required)

  apps:
    - path: apps/web            # name defaults to the dir: "web"

    - name: api
      path: apps/api
      namespace: shop           # default namespace for this app
      needs: [db]               # sync only after db is Healthy
      build:                    # build from source (ksync help builds)
        - image: ghcr.io/app/api
          context: ../src/api

    - name: db
      path: apps/postgres
      namespace: shop

## Required
  allowedContexts   kubectl context names ksync may target. This is the safety gate.
  apps[].path       a directory with a kustomization file.

## Common app fields
  name         app name (default: the directory name).
  namespace    default namespace for resources that set none.
  needs        apps that must be Healthy before this one syncs.
  build        images built from local source (ksync help builds).

## Requirements
  A local cluster and its kubectl context. helm on PATH only when a kustomization
  inflates helmCharts (kustomize is built in). docker only when an app builds.

─── Advanced ──────────────────────────────────────────────

## Context selection (the safety gate)
  ksync targets only a listed context and never reads your kubeconfig current-context.
  One concrete entry is used automatically. With several entries, or a glob, you must
  pass --context <name>, and it must match an entry. Entries are shell globs, so 'k3s-*'
  matches a family of clusters. '*' matches everything and removes the safety gate.

## Namespaces
  namespace sets the default for rendered resources that declare none (like ArgoCD's
  destination.namespace). ksync creates a namespace if it is missing, but never labels,
  prunes, or deletes one.

## Per-environment patches
  patches edits a rendered field that the kustomization cannot set per environment (for
  example a hostPath that depends on where ksync runs). It runs after render, before
  image injection, and must match exactly one object.

    patches:
      - target: { kind: Deployment, name: cache }
        patch: |
          - op: replace
            path: /spec/template/spec/volumes/0/hostPath/path
            value: ${HOME}/.cache/web

  patch is an inline RFC 6902 op list. ${VAR} in a value expands at render time:
  ${VAR:-default} falls back when unset or empty, a bare undefined ${VAR} fails the render,
  and $$ is a literal $. ${KSYNC_WORKDIR} is the config directory.

## More app fields
  clientRender             render this app client-side (ksync help strategy).
  imageLoad, buildGroups   top-level build helpers (ksync help builds).

Unknown fields are rejected.`

const buildsTopic = `A build entry rebuilds an image from local source on change, tags it by content,
injects the tag, and rolls the pods.

  build:
    - image: ghcr.io/app/api    # the ref your manifests use, without a tag
      context: ../src/api       # docker build context; watched for changes

## Fields
  image         image name as manifests reference it, no tag. Required.
  context       build context directory, relative to the config. Watched. Required.
  name          label shown in progress output. Default: the image's last segment.
  dockerfile    Dockerfile relative to context. Default: Dockerfile.
  watch         paths under context that trigger a rebuild. Default: all of context.
  watchIgnore   paths kept in the build but excluded from triggering a rebuild.

## Good to know
  - The tag is 'ksync-<hash of the image>', so identical content keeps the same tag and
    does not restart the pods.
  - imagePullPolicy: Always on a built image becomes IfNotPresent (the local tag is in no
    registry, so a pull would fail).
  - On kind, k3d, or a remote cluster, built images must be loaded. See imageLoad below.

─── Advanced ──────────────────────────────────────────────

## Load built images into the cluster (imageLoad)
  ksync builds into the local docker daemon. Docker Desktop runs pods from it directly.
  kind, k3d, and remote clusters keep a separate store, so a built tag must be loaded.
  The command runs only when an image is rebuilt.

    imageLoad:
      command: k3d image import --cluster dev $KSYNC_IMAGES
      # kind:     kind load docker-image --name dev $KSYNC_IMAGES
      # registry: for i in $KSYNC_IMAGES; do docker push "$i"; done

  It receives $KSYNC_IMAGES (refs, newline-separated), $KSYNC_IMAGE (the first), and
  $KSYNC_CONTEXT. With kind or k3d, set pods to imagePullPolicy: Never or IfNotPresent.

## Custom build command
  command replaces docker build for one image. It runs via 'sh -c' in the context and
  must leave the image tagged as "$KSYNC_IMAGE". It excludes dockerfile and group.

## Bulk builds (buildGroups)
  A buildGroups entry builds several images with one command (for example a
  'docker buildx bake', or a compile that produces many binaries). Members reference the
  group by name.

    buildGroups:
      - name: services
        command: docker buildx bake --load ...   # tag each as <image>:ksync-build
    apps:
      - name: services
        path: manifests/services
        build:
          - { image: ghcr.io/app/api, context: ../.., group: services }
          - { image: ghcr.io/app/web, context: ../.., group: services }

  The command gets $KSYNC_IMAGES, $KSYNC_IMAGE, and $KSYNC_CONTEXT, and must tag each
  requested image as <image>:ksync-build. ksync content-tags them afterward. A group's
  command runs one app at a time by default; set 'parallel: true' when it is safe to run
  concurrently.

## Deploy a pre-built image (overrides)
  'ksync sync --image IMAGE=REF' (or KSYNC_IMAGE_OVERRIDES, also on diff) deploys an
  existing image instead of building it. REF may be a bare tag, name:tag, or @digest. The
  build and its imageLoad are skipped. watch does not accept overrides.`

const strategyTopic = `ksync runs three phases. Each defaults to cluster-aware behavior, with an opt-out for
when that default is wrong for one app or one run.

  Phase    Default                                     Opt-out
  Render   helm template against the cluster           clientRender: true (per app)
                                                       --offline-render (per run)
  Diff     server-side dry-run apply (no false drift)  --client-diff (per run)
  Apply    server-side apply; prune by ksync.dev/app   --prune=false

## Safety
  - ksync targets only an allowedContexts entry, never your kubeconfig current-context.
  - It labels every applied resource (except Namespaces) ksync.dev/app: <app>. Prune and
    destroy touch only resources with that label. Everything else is invisible to them.
  - Namespaces are never labeled, pruned, or deleted.
  - destroy requires --yes and prints its scope first.
  - An app that renders nothing while it still owns live resources will not prune them.
    ksync stops and points you at 'ksync destroy <app>'.

─── Advanced ──────────────────────────────────────────────

## clientRender: true (per app)
  The default render is a server-side dry-run, so the cluster must know every kind. An app
  whose chart ships a CRD together with custom resources of that kind fails with 'no
  matches for kind' until the CRD exists. clientRender renders that app client-side (the
  way ArgoCD does), so it can carry both. It still uses the cluster's capabilities; only
  --offline-render skips the cluster entirely. It disables helm lookup for the app, so use
  it only where the chart does not need lookup.

## --offline-render (per run)
  Renders with no cluster at all (plain helm; lookup returns empty). It byte-matches
  'kustomize build --enable-helm --load-restrictor LoadRestrictionsNone', and overrides
  clientRender.

## --client-diff (per run)
  The server-side dry-run hides fields the apiserver defaults or prunes, so they do not
  look like drift. --client-diff uses a faster in-process diff, but then such a field
  shows as a permanent change.

## Output
  Manifests go to stdout; status goes to stderr. Color is on for a terminal, off under
  NO_COLOR. On a terminal each app is a live pipeline (Build -> Import -> Deploy); a pipe
  or CI streams each build's output line by line instead.`

const hooksTopic = `ksync follows ArgoCD's hook rules, not Helm's. You need this only if your charts use
hooks.

A sync wave is an ordering group: ksync applies one wave, waits for it to be Healthy,
then applies the next.

  - Hooks are read from argocd.argoproj.io/hook, falling back to helm.sh/hook (excluding
    crd-install).
  - post-install and post-upgrade become PostSync. They run on every sync, after the main
    resources are Healthy.
  - helm.sh/hook-weight is the sync wave when argocd.argoproj.io/sync-wave is absent.
  - helm.sh/hook-delete-policy maps as in ArgoCD (default BeforeHookCreation).

A no-change sync skips hooks, unless a hook's Job is currently failed (it re-runs so a
transiently-failed PostSync self-heals), or --force re-runs them all.`

const troubleshootingTopic = `Most errors name their own fix. The common ones:

## <app> rendered 0 resources but manages N live resource(s)
  The kustomization now renders nothing (a typo, a dropped resources: entry). Fix it, or
  run 'ksync destroy <app>' to remove the app.

## cluster cannot map X
  A live render asks the cluster to map every kind. A missing CRD, or an apiVersion the
  cluster does not serve, makes it fail. Install the CRD, set clientRender: true on the app
  if it bundles its own CRDs, or use --offline-render (ksync help strategy).

## Pods of a built image show ErrImagePull
  The kubelet tried to pull the local ksync-... tag. Either the image has no build entry,
  or the cluster has a separate store with no imageLoad (ksync help builds).

## Pods restart on every rebuild with no source change
  ksync tags an image by a fingerprint of its layers and runtime config, so a rebuild that
  changes neither keeps the same tag. If pods still roll, the build bakes something new in
  every time (often a timestamp or random value in a layer or a label). Make that part
  reproducible.

## A change is not picked up by watch
  ksync watches each app's directory, the paths its kustomization references, and each
  build's context (or its watch: roots), minus .dockerignore and watchIgnore. For anything
  outside that, run 'ksync sync'.

## First sync after start is slow
  ksync lists the cluster once to warm its cache. Keep watch running: later syncs in the
  same session reuse it, while a one-shot 'ksync sync' warms it again each run.`

// helpTopic is one concept page. Running 'ksync <name>' prints its guide, and
// 'ksync help <name>' shows the same text through cobra's help. It carries a Run
// (not merely a Long) so cobra lists it under the topics group rather than its
// catch-all "Additional help topics" section.
func helpTopic(name, short, long string) *cobra.Command {
	cmd := &cobra.Command{
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
	// A concept guide is prose, not an invocable command: print just the guide, with
	// none of cobra's "Usage:/Flags:" trailer (there are no real flags — it is only
	// noise on a guide). This makes all three paths render the identical clean page:
	// 'ksync <topic>' (RunE), 'ksync help <topic>', and 'ksync <topic> -h'.
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		_, _ = fmt.Fprintln(c.OutOrStdout(), c.Long)
	})
	return cmd
}

// helpTopics returns the concept pages, in the order the root help lists them.
func helpTopics() []*cobra.Command {
	return []*cobra.Command{
		helpTopic("config", "write your ksync.yaml: apps, contexts, namespaces", configTopic),
		helpTopic("builds", "build images from source, and load them into the cluster", buildsTopic),
		helpTopic("strategy", "how render, diff, apply, and prune behave", strategyTopic),
		helpTopic("hooks", "ArgoCD hooks and sync waves", hooksTopic),
		helpTopic("troubleshooting", "common errors and their fixes", troubleshootingTopic),
	}
}
