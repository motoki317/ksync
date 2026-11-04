package main

import (
	"fmt"
	"io"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
	"github.com/motoki317/ksync/internal/ui"
)

// Resolve once before builds or deploys start, so watch keeps the same tags for
// its whole session.
func addNeededLiveTags(cfg *config.Config, apps []config.App, overrides map[string]render.Image, liveObjects func(string, string, []*unstructured.Unstructured) ([]*unstructured.Unstructured, error)) error {
	selected := make(map[string]bool, len(apps))
	for _, app := range apps {
		selected[app.Name] = true
	}
	owners := make(map[string]config.App, len(cfg.Apps))
	for _, app := range cfg.Apps {
		owners[app.Name] = app
	}
	tagsByOwner := make(map[string]map[string]string)
	for _, app := range apps {
		for _, ref := range cfg.NeededBuilds(app) {
			if selected[ref.App] {
				continue
			}
			if _, supplied := overrides[ref.Image]; supplied {
				continue
			}
			tags, read := tagsByOwner[ref.App]
			if !read {
				owner := owners[ref.App]
				var err error
				tags, err = liveBuildTags(owner, liveObjects)
				if err != nil {
					return fmt.Errorf("live tags for need %s of app %s: %w", owner.Name, app.Name, err)
				}
				tagsByOwner[ref.App] = tags
			}
			if tag, found := tags[ref.Image]; found {
				overrides[ref.Image] = render.Image{Name: ref.Image, NewTag: tag}
			}
		}
	}
	return nil
}

func liveBuildTags(app config.App, liveObjects func(string, string, []*unstructured.Unstructured) ([]*unstructured.Unstructured, error)) (map[string]string, error) {
	live, err := liveObjects(app.Name, app.Namespace, nil)
	if err != nil {
		return nil, err
	}
	repos := make([]string, len(app.Build))
	for i, b := range app.Build {
		repos[i] = b.Image
	}
	return render.LiveDevTags(live, repos), nil
}

// Pass the returned callback the referenced repos after patches and before SetImages,
// so notes match the manifest refs. Each fallback prints once per watch session.
func neededImageNotes(cfg *config.Config, apps []config.App, overrides map[string]render.Image, notes io.Writer) func(string, map[string]bool) {
	selected := make(map[string]bool, len(apps))
	for _, app := range apps {
		selected[app.Name] = true
	}
	pending := make(map[string][]config.BuildRef)
	for _, app := range apps {
		for _, ref := range cfg.NeededBuilds(app) {
			if _, supplied := overrides[ref.Image]; !selected[ref.App] && !supplied {
				pending[app.Name] = append(pending[app.Name], ref)
			}
		}
	}
	if len(pending) == 0 {
		return nil
	}
	var mu sync.Mutex
	return func(app string, referenced map[string]bool) {
		mu.Lock()
		defer mu.Unlock()
		refs := pending[app]
		remaining := refs[:0]
		for _, ref := range refs {
			if referenced[ref.Image] {
				ui.WriteLine(ui.SectionLog, notes, fmt.Sprintf("note: %s deploys the manifest ref of %s, because %s is not selected and runs no single ksync dev tag of it. Select %s too, or pass --image %s=REF.\n", app, ref.Image, ref.App, ref.App, ref.Image))
			} else {
				remaining = append(remaining, ref)
			}
		}
		pending[app] = remaining
	}
}
