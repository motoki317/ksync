package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/render"
)

// runImages prints the canonical container image references the selected apps
// deploy, one per line, sorted and deduplicated — the set a cache/pre-pull tool
// scopes to so it never caches more than ksync.yaml actually uses.
//
// References are normalized to their fully-qualified form (`redis:7` ->
// `docker.io/library/redis:7`, an untagged ref -> `:latest`) — the same
// normalization containerd applies — so the output matches the cluster image
// store exactly and a consumer can do plain set membership without re-parsing.
//
// Images ksync builds locally (any `build:` entry's image) are excluded: those
// are content-addressed dev tags that live only in the local store and are never
// pulled, so caching them is pointless. With --live, images of running pods in
// the apps' namespaces are added too, which is how operator-derived images (an
// ECK Elasticsearch data image named only by spec.version) get covered.
func runImages(args []string) error {
	fs := flag.NewFlagSet("images", flag.ContinueOnError)
	live := fs.Bool("live", false, "also include images of running pods in the apps' namespaces (captures operator-derived images, e.g. ECK Elasticsearch, that rendered manifests never name)")
	offline := fs.Bool("offline-render", false, "render helm charts without live-cluster lookup (charts using helm `lookup` will not resolve)")
	maxParallel := fs.Int("max-parallel", runtime.NumCPU(), "how many apps to render concurrently (0 = one worker per app)")
	kctx := contextFlag(fs)
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	kubeContext, err := resolveContext(cfg, *kctx)
	if err != nil {
		return err
	}

	// Exclude every image any app builds (not just the selected ones): a built
	// image is a local-only dev tag, never something to cache, wherever it appears.
	built := builtRepos(cfg)
	images := newImageSet(built)

	renderOpts, cleanup, err := renderOptions(kubeContext, *offline)
	if err != nil {
		return err
	}
	defer cleanup()
	r := render.New(renderOpts)
	lookup := render.NewVarLookup(cfg.Dir())
	// Render apps concurrently (the slow part is a helm dry-run per chart
	// release); merge the per-app results sequentially afterward so the imageSet
	// needs no locking.
	type appResult struct {
		refs       []string
		namespaces []string
	}
	rendered, err := renderConcurrently(apps, *maxParallel, func(app config.App) (appResult, error) {
		res, err := r.Render(app.Path)
		if err != nil {
			return appResult{}, fmt.Errorf("app %s: %w", app.Name, err)
		}
		// A patch could change an image ref, so apply before reading the image set
		// — otherwise `images` would report a ref sync does not deploy.
		if err := res.ApplyPatches(app.Patches, lookup); err != nil {
			return appResult{}, fmt.Errorf("app %s: %w", app.Name, err)
		}
		ns := map[string]struct{}{}
		collectNamespaces(ns, res.Objects, app.Namespace)
		nsList := make([]string, 0, len(ns))
		for n := range ns {
			nsList = append(nsList, n)
		}
		return appResult{refs: res.Images(), namespaces: nsList}, nil
	})
	if err != nil {
		return err
	}
	namespaces := map[string]struct{}{}
	for _, ar := range rendered {
		for _, ref := range ar.refs {
			images.add(ref)
		}
		for _, n := range ar.namespaces {
			namespaces[n] = struct{}{}
		}
	}

	if *live {
		ctx, stop := signalContext()
		defer stop()
		if err := addLiveImages(ctx, kubeContext, namespaces, images); err != nil {
			return err
		}
	}

	for _, ref := range images.sorted() {
		if _, err := fmt.Fprintln(os.Stdout, ref); err != nil {
			return err
		}
	}
	return nil
}

// builtRepos is the set of canonical repositories ksync builds locally, used to
// drop them from the image list.
func builtRepos(cfg *config.Config) map[string]bool {
	built := make(map[string]bool)
	for _, a := range cfg.Apps {
		for _, b := range a.Build {
			if repo, _, ok := normalizeRef(b.Image); ok {
				built[repo] = true
			}
		}
	}
	return built
}

// collectNamespaces records the namespaces a rendered app deploys into: each
// object's own namespace, falling back to the app's default for namespaced
// objects that leave it unset (the namespace they are applied into). Cluster-
// scoped objects contribute nothing. These bound the --live pod query so it
// reads only namespaces ksync manages, never k3s system pods.
func collectNamespaces(into map[string]struct{}, objs []*unstructured.Unstructured, fallback string) {
	for _, obj := range objs {
		if ns := obj.GetNamespace(); ns != "" {
			into[ns] = struct{}{}
		}
	}
	if fallback != "" {
		into[fallback] = struct{}{}
	}
}

// addLiveImages adds the container images of running pods in the given
// namespaces. This catches images no manifest names because a controller
// creates the pod (an ECK operator materializing an Elasticsearch's data image
// from spec.version) — exactly the ones a render-only list misses.
func addLiveImages(ctx context.Context, kubeContext string, namespaces map[string]struct{}, images *imageSet) error {
	cfg, err := engine.RESTConfig(kubeContext)
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building kubernetes client: %w", err)
	}
	for ns := range namespaces {
		pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("listing pods in %s: %w", ns, err)
		}
		for i := range pods.Items {
			addPodImages(images, &pods.Items[i])
		}
	}
	return nil
}

// addPodImages adds every container image declared on a pod (the spec refs are
// stable and canonicalizable, unlike the status refs which a runtime may pin to
// a digest).
func addPodImages(images *imageSet, pod *corev1.Pod) {
	for _, c := range pod.Spec.InitContainers {
		images.add(c.Image)
	}
	for _, c := range pod.Spec.Containers {
		images.add(c.Image)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		images.add(c.Image)
	}
}

// imageSet accumulates canonical image references, dropping unparseable refs and
// any whose repository ksync builds locally.
type imageSet struct {
	built map[string]bool
	refs  map[string]struct{}
}

func newImageSet(built map[string]bool) *imageSet {
	return &imageSet{built: built, refs: map[string]struct{}{}}
}

func (s *imageSet) add(ref string) {
	repo, full, ok := normalizeRef(ref)
	if !ok || s.built[repo] {
		return
	}
	s.refs[full] = struct{}{}
}

func (s *imageSet) sorted() []string {
	out := make([]string, 0, len(s.refs))
	for ref := range s.refs {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// normalizeRef parses an image reference and returns its canonical repository
// (registry/name, no tag) and canonical full reference (with an explicit
// :latest when neither tag nor digest is given) — the exact normalization
// containerd uses, so the output matches `ctr images ls`. ok is false for a ref
// that does not parse.
func normalizeRef(ref string) (repo, full string, ok bool) {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(ref))
	if err != nil {
		return "", "", false
	}
	return named.Name(), reference.TagNameOnly(named).String(), true
}
