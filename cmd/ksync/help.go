package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// This file holds ksync's help prose: the root description, each command's Long
// text and Example block, and the concept-topic pages reachable via
// `ksync help <topic>`. The help IS the user guide, so keeping the copy here (not
// scattered across the command constructors in cli.go) makes the whole manual
// reviewable in one place.

const rootLong = `ksync is a local-development sync loop for Kubernetes.

It watches local kustomize directories, called apps. When an app changes, ksync syncs
it to a local cluster: it renders the manifests, diffs them against the cluster,
applies the changes, and waits until the resources are healthy. A sync follows the
ArgoCD rules for hooks, sync waves, and prune (ksync deletes the resources that you
removed from the manifests). ksync can also build images from source and redeploy them.

The commands act on the selected apps. These are the apps that you name. With no
names, they are the apps without profiles, plus the apps in an active profile.

First run:

  1. Write a ksync.yaml.    'ksync help config' lists every field.
  2. ksync diff             Preview what a sync changes.
  3. ksync sync             Sync once.
  4. ksync watch            Sync again on every change.`

// Per-command Long text and Examples. Each is a self-contained page: what the
// command does, its sharp edges, and worked invocations — the detail the flag
// list alone cannot carry. Concept material common to several commands lives in
// the help topics below, linked by name.

const watchLong = `Sync the selected apps once, then sync each app again when its files change. If the
source of a built image changes, watch rebuilds the image first.

On a terminal, watch asks what to rebuild or redeploy after each change. With --auto,
or without a terminal, watch does not ask.

A failed sync does not stop watch. If an app is not healthy within --timeout, watch
prints diagnostics, then retries the app with a growing delay.

See 'ksync help builds' and 'ksync help strategy'.`

const watchExample = `  ksync watch                # sync, then watch for changes
  ksync watch web api        # only these two apps
  ksync watch --auto         # act on every change without asking`

const syncLong = `Sync the selected apps once, then exit. An app with build entries builds its images
first.

Apps run in parallel. An app starts only after the apps it needs are healthy. If one
app fails, sync stops the other apps and exits with an error.

If an app is not healthy within --timeout, sync fails and prints diagnostics: the
resources that are not healthy, their warning events, and pod logs. To stop a stuck
sync early and print the same diagnostics, press Ctrl-C.

See 'ksync help strategy' and 'ksync help hooks'.`

const syncExample = `  ksync sync                                  # sync once
  ksync sync -p debug                         # also sync apps in the debug profile
  ksync sync web                              # only the web app
  ksync sync --force                          # run hooks again, even with no change
  ksync sync --image ghcr.io/app/api=pr-42    # deploy tag pr-42 of api, no build`

const diffLong = `Preview a sync: print a unified YAML diff for each resource that a sync creates,
updates, or prunes. diff changes nothing. Secret values are masked.

diff renders like sync, with the same patches and image overrides. It does not build,
so a built image keeps the dev tag that the cluster runs now.

See 'ksync help strategy' for how ksync computes the diff.`

const diffExample = `  ksync diff             # preview the changes
  ksync diff web api     # only these apps`

const renderLong = `Print the rendered manifests of the selected apps to stdout. Status goes to stderr, so
'ksync render web > web.yaml' writes clean YAML.

render applies patches but does not build, so a built image shows the ref from the
manifests, not a dev tag.

For helmCharts, render uses the cluster by default, so helm lookup and capabilities
work as they do in a sync. With --offline-render it needs no cluster, and its output
matches 'kustomize build --enable-helm --load-restrictor LoadRestrictionsNone' byte
for byte.`

const renderExample = `  ksync render web > web.yaml
  ksync render --offline-render web    # no cluster needed`

const imagesLong = `Print the images that the selected apps deploy, one per line, sorted and unique. Each
is a full reference as containerd stores it (docker.io/library/redis:7, not redis:7),
so a cache or pre-pull tool can match the lines as plain strings.

Images that ksync builds are left out, because no registry has their dev tags.

--live adds the images of the pods in the namespaces of the apps. This catches images
that an operator picks and the manifests never name.`

const imagesExample = `  ksync images          # the images that the apps deploy
  ksync images --live   # also the images of running pods`

const destroyLong = `Delete every resource that ksync tracks (label ksync.dev/app) for the selected apps,
in reverse needs order. Namespaces are never deleted. destroy stops at the first app
that fails. Without --yes, it prints the apps and the context, and deletes nothing.`

const destroyExample = `  ksync destroy web          # print what it targets, delete nothing
  ksync destroy web --yes    # delete the resources of web
  ksync destroy -p '*' --yes # delete the resources of every app`

// Concept topics. Each is a help-only command: 'ksync <topic>' prints its Long and
// 'ksync help <topic>' shows the same text. They do no cluster work but carry a Run
// (see helpTopic). They hold the ksync.yaml schema and the behavior no per-flag
// description can express.

const configTopic = `ksync reads ksync.yaml from the current directory, or the file that -f names. App paths
and build contexts are relative to the directory of that file. Unknown fields are
errors.

## Example

  allowedContexts:
    - docker-desktop

  apps:
    - path: apps/web            # app name "web", from the directory

    - name: api
      path: apps/api
      namespace: shop
      needs: [db]
      build:
        - image: ghcr.io/app/api
          context: src/api

    - name: db
      path: apps/postgres
      namespace: shop

## Top-level fields
  allowedContexts   kubectl contexts ksync can use. Required. See Contexts below.
  apps              the apps. Required.
  imageLoad         loads built images into the cluster ('ksync help builds').
  buildGroups       builds several images with one command ('ksync help builds').

## App fields
  path              a directory with a kustomization file. Required.
  name              the app name. Default: the directory name.
  namespace         the namespace for resources that set none.
  needs             apps that must be healthy before this app syncs.
  profiles          optional groups, for example [debug]. See Profiles below.
  build             images built from local source ('ksync help builds').
  patches           edits to rendered objects. See Patches below.
  clientRender      render helm charts client-side ('ksync help strategy').

## Requirements
  A local cluster and its kubectl context. helm 3.17 or later on PATH, only if a
  kustomization uses helmCharts (kustomize is built in). docker, only if an app has
  build entries.

─── Advanced ──────────────────────────────────────────────

## Contexts (the safety gate)
  ksync never reads the current-context of your kubeconfig. If allowedContexts has one
  entry that is not a glob, ksync uses it. Otherwise pass --context, which must match
  an entry. Entries are shell globs: 'k3s-*' matches a family of clusters, and '*'
  matches every context, which removes the gate.

## Profiles
  Activate profiles with -p, or set a comma-separated default in KSYNC_PROFILES. Any
  -p replaces that default, so -p '' activates no profile.

  App names select exactly those apps and ignore profiles. ksync does not sync the apps
  that the named apps need, and assumes that those apps are already deployed. sync
  prints a note about it.

  With no app names, the apps that a selected app needs must be selected too.
  Otherwise the command fails before it touches the cluster.

  Leaving an app out of the selection never deletes its resources. To delete them, use
  'ksync destroy'.

## Patches
  patches sets a rendered field that the kustomization cannot, such as a hostPath that
  depends on the machine.

    patches:
      - target: { kind: Deployment, name: cache }
        patch: |
          - op: replace
            path: /spec/template/spec/volumes/0/hostPath/path
            value: ${HOME}/.cache/web

  target takes kind and name, and optional group, version, and namespace. It must
  match exactly one rendered object. Its namespace matches the namespace in the
  rendered manifest, not the namespace field of the app. patch is a block string that
  holds a list of RFC 6902 operations.

  ksync expands ${VAR} only in the value of an operation, never in its path. Values
  come from the environment. ${VAR:-default} uses the default when VAR is unset or
  empty. An unset ${VAR} with no default is an error. $$ is a literal $.
  ${KSYNC_WORKDIR} is the directory of ksync.yaml.`

const buildsTopic = `A build entry builds an image from local source. When the source changes, ksync
rebuilds the image, gives it a dev tag, and syncs the app with that tag.

  build:
    - image: ghcr.io/app/api
      context: src/api

## Fields
  image         the image name as the rendered manifests use it (after any kustomize
                images: rewrite), without a tag. Required.
  context       the build context directory. Required.
  name          the label in progress output. Default: the last part of image.
  dockerfile    the Dockerfile, relative to context. Default: Dockerfile.
  watch         paths, relative to context, that trigger a rebuild. Default: context.
  watchIgnore   paths that stay in the build but trigger no rebuild (.dockerignore
                syntax), such as a binary that a host compile writes into context.
  command       a shell command instead of docker build. See below.
  group         the buildGroups entry that builds this image. See below.

## Good to know
  - The dev tag is ksync-<hash of the image content>. A rebuild with the same content
    keeps the tag, so the pods do not restart.
  - Files that .dockerignore excludes never trigger a rebuild.
  - ksync changes imagePullPolicy: Always to IfNotPresent on built images, because no
    registry has the dev tag.
  - Only one app can build an image, and only that app gets the dev tag.

## Load built images into the cluster (imageLoad)
  ksync builds into your local docker image store. Docker Desktop runs pods from that
  store. Other clusters keep their own image store, for example kind, k3d, k3s,
  minikube, and remote clusters. For them, set a top-level imageLoad command. A pod in
  ErrImagePull on a ksync-<hash> tag is the sign that the cluster needs one.

    imageLoad:
      command: k3d image import --cluster dev $KSYNC_IMAGES
      # kind:     kind load docker-image --name dev $KSYNC_IMAGES
      # k3s:      docker save $KSYNC_IMAGES | k3s ctr -n k8s.io images import -
      # registry: for i in $KSYNC_IMAGES; do docker push "$i"; done

  The command runs after a build, for the images that this ksync process has not
  loaded yet. So each 'ksync sync' run loads its images again. $KSYNC_IMAGES holds the
  refs, one per line, and one call can carry several images.

  ksync runs every command in ksync.yaml with sh -c. $KSYNC_CONTEXT holds the kubectl
  context.

─── Advanced ──────────────────────────────────────────────

## Custom build command (command)
  command replaces docker build for one image. It runs in the context directory and
  must tag the image as $KSYNC_IMAGE, a temporary ref of the form <image>:ksync-build.
  ksync then retags the image with its dev tag.

## Build several images with one command (buildGroups)
  A buildGroups entry builds several images with one command, such as a
  'docker buildx bake'. A build entry joins it with group: <name>. All members must use
  the same context, where the command runs.

    buildGroups:
      - name: services
        command: |
          sets= targets=
          for ref in $KSYNC_IMAGES; do
            t=${ref##*/}; t=${t%:ksync-build}
            sets="$sets --set $t.tags=$ref"; targets="$targets $t"
          done
          docker buildx bake --load $sets $targets
    # in an app:
    build:
      - { image: ghcr.io/app/api, context: ., group: services }
      - { image: ghcr.io/app/web, context: ., group: services }

  $KSYNC_IMAGES lists the temporary ref of each member that needs a rebuild, one per
  line. The command must tag each image with its ref, and then ksync retags it with its
  dev tag. The example assumes that each bake target has the name of the last part of
  its image.

  By default, runs of one group never overlap, even across apps. If the command is
  safe to run in parallel, set parallel: true on the group.

## Deploy a pre-built image (image override)
  --image IMAGE=REF on sync, diff, or watch deploys an existing image instead of
  building it. ksync does not run imageLoad for it, so REF must already be in the image
  store of the cluster, or the cluster must be able to pull it. KSYNC_IMAGE_OVERRIDES
  takes the same pairs, separated by spaces or newlines. A flag wins over the variable.
  REF is a tag, name:tag, name@digest, or @digest. ksync ignores an override for an
  image that no app builds.

  sync and diff use the override for the whole run. watch uses it until the first edit
  to the source of the image, then builds the image as usual.`

const strategyTopic = `## Sync phases
  Phase    Default                                   Opt-out
  Render   helm renders charts against the cluster   clientRender: true (per app)
                                                     --offline-render (per run)
  Diff     dry-run apply on the apiserver            --client-diff (per run)
  Apply    server-side apply                         ServerSideApply=false in the
                                                     argocd.argoproj.io/sync-options
                                                     annotation (per resource)
  Prune    delete tracked resources that are no      --prune=false (per run)
           longer rendered

The Render row matters only for helmCharts. ksync renders plain kustomize without the
cluster.

## Safety
  - ksync labels each resource that it applies with ksync.dev/app: <app>. Prune and
    destroy touch only resources with that label.
  - The label holds only the app name, and prune matches it in the whole cluster. Two
    ksync.yaml files with the same app name on one cluster prune each other's
    resources. Before you rename an app, destroy it, or its old resources stay behind.
  - ksync takes over an existing resource with the same kind, namespace, and name, for
    example one from an earlier helm or kubectl deploy. It applies over it and adds its
    label.
  - ksync creates a missing namespace that an app uses, but never labels, prunes, or
    deletes a namespace.
  - If an app renders nothing but still owns live resources, the sync fails instead of
    pruning them all.

## Retries
  A sync retries each failed apply until the app is healthy or --timeout runs out, and
  prints each failure and recovery. So a CRD that is still registering, or a webhook
  that is not serving yet, heals by itself.

─── Advanced ──────────────────────────────────────────────

## clientRender: true (per app)
  By default, helm renders charts with --dry-run=server against the cluster, so the
  cluster must know every kind. A chart that ships a CRD and resources of that kind
  then fails with "no matches for kind" until the CRD exists. clientRender renders the
  charts of the app client-side, as ArgoCD does. It still uses the version and API list
  of the cluster, but helm lookup does not work.

## --offline-render (per run)
  Render charts with no cluster: plain helm template, where lookup returns nothing.
  This overrides clientRender.

## --client-diff (per run)
  By default, the diff is a dry-run apply on the apiserver. It returns the object that
  the apiserver would store, so fields that the apiserver sets or drops are not drift.
  --client-diff diffs in-process. It is faster, but diff then shows such fields as a
  change, and sync applies them every time.

## Output
  NO_COLOR turns color off.`

const hooksTopic = `ksync follows ArgoCD hook and sync-wave rules, not Helm rules. They apply to every
rendered resource, from a chart or from plain YAML.

A sync wave is an ordering group, set with argocd.argoproj.io/sync-wave (default 0).
ksync applies the waves from the lowest number up. It waits until one wave is healthy
before it applies the next.

  - The hook type comes from argocd.argoproj.io/hook, else from helm.sh/hook. A Helm
    crd-install hook is a normal resource.
  - pre-install and pre-upgrade become PreSync. post-install and post-upgrade become
    PostSync, which runs after the main resources are healthy.
  - A resource with only other helm.sh/hook values (test, rollback, and delete hooks)
    is never applied.
  - helm.sh/hook-weight is the sync wave when argocd.argoproj.io/sync-wave is absent.
  - helm.sh/hook-delete-policy maps to the ArgoCD policy (default BeforeHookCreation).

Hooks run on every sync that changes something, not only on install. A sync with no
change skips them, with two exceptions. If a hook is still failed from an earlier sync,
all hooks run again. --force always runs them all.`

const troubleshootingTopic = `## sync of "<app>" timed out
  The app was not healthy within --timeout. Start with the last apply error and the
  resources that the message names, then the Diagnostics block (warning events, pod
  logs). Common causes: a CRD that is never installed, a webhook that never starts, or a
  workload stuck in ErrImagePull or CrashLoopBackOff. If the app is only slow, raise
  --timeout.

## Pods of a built image show ErrImagePull
  If the pod asks for a ksync-<hash> tag, set imageLoad ('ksync help builds'). If it
  asks for another tag, the app that runs the pod has no build entry for that image
  name.

## Pods restart on every rebuild, with no source change
  The same content keeps the same dev tag. A restart means that the build puts something
  new into the image each time, often a timestamp or a random value in a layer or label.
  Make that part reproducible.

## watch does not pick up a change
  watch reads ksync.yaml once, at start. After you edit ksync.yaml, restart watch.

  watch sees each app directory, the paths that its kustomization references, and each
  build context (or its watch paths), minus .dockerignore and watchIgnore. For other
  changes, run 'ksync sync'.

## The first sync is slow
  At start, ksync lists the cluster once to fill its cache. Later syncs in one watch
  reuse it. Each 'ksync sync' fills it again.`

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
		helpTopic("config", "write ksync.yaml: apps, contexts, profiles, patches", configTopic),
		helpTopic("builds", "build images from source and load them into the cluster", buildsTopic),
		helpTopic("strategy", "how a sync renders, diffs, applies, and prunes", strategyTopic),
		helpTopic("hooks", "ArgoCD hooks and sync waves", hooksTopic),
		helpTopic("troubleshooting", "common problems and their fixes", troubleshootingTopic),
	}
}
