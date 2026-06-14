// Package loop is the watch-mode heart of ksync: it wires the file watcher,
// the dirty-set mapping, the scheduler, the builder, and the renderer into
// one event loop that builds, renders, and syncs apps as their inputs change.
// The cluster and docker sides are injected as SyncFunc/BuildFunc so the loop
// is testable without either.
package loop

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/build"
	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
	"github.com/motoki317/ksync/internal/schedule"
	"github.com/motoki317/ksync/internal/ui"
	"github.com/motoki317/ksync/internal/watch"
)

// SyncStats summarizes one app's apply for the loop's status line: how many
// objects the sync changed, pruned, or failed on, and how many of its live
// resources are Degraded afterwards. Applied counts only objects that actually
// differed, so a no-op re-sync reports 0 — the developer sees at a glance
// whether their edit changed anything. Degraded is a post-sync health snapshot
// (genuinely broken, not a rollout in flight); a healthy edit reports 0.
type SyncStats struct{ Applied, Pruned, Failed, Degraded int }

// SyncFunc applies one app's rendered objects to the cluster and reports how
// many changed, were pruned, or failed.
type SyncFunc func(ctx context.Context, app string, objects []*unstructured.Unstructured) (SyncStats, error)

// BuildFunc produces the images of one build batch and returns their full
// content-addressed refs in the same order. A batch is either a single
// ungrouped entry or all the dirty members of one build group (built by one
// bulk command); the loop forms the batches via App.BuildBatches.
type BuildFunc func(ctx context.Context, builds []config.Build) ([]string, error)

// Options tune the loop; zero values get sensible watch-mode defaults.
type Options struct {
	Debounce    time.Duration // default 200ms
	MaxParallel int           // default 4
	RetryBase   time.Duration // default 1s
	RetryMax    time.Duration // default 2m
	Render      render.Options
	Build       BuildFunc // required when any app declares builds
	Log         logr.Logger
	// Resync, when a value is received, marks every app dirty — the manual
	// "redeploy everything now" the watch command wires to keyboard input.
	// A nil channel simply never fires.
	Resync <-chan struct{}
}

func (o *Options) applyDefaults() {
	if o.Debounce <= 0 {
		o.Debounce = 200 * time.Millisecond
	}
	if o.MaxParallel <= 0 {
		o.MaxParallel = 4
	}
	if o.RetryBase <= 0 {
		o.RetryBase = time.Second
	}
	if o.RetryMax <= 0 {
		o.RetryMax = 2 * time.Minute
	}
	if o.Log.GetSink() == nil {
		o.Log = logr.Discard()
	}
}

// buildState is the loop's per-build-entry memory: whether sources changed
// since the last successful build, and the last built tag (re-injected on
// every render until a newer build replaces it).
type buildState struct {
	scope *build.Scope
	dirty bool
	tag   string
}

// Run watches the apps' inputs and builds+renders+syncs them on change until
// ctx is done. Every app is synced — and every build entry built — once at
// startup, so the cluster converges to the current working tree before
// incremental behavior takes over. Startup builds are how ksync avoids
// persisting build state: unchanged sources hit the docker layer cache and
// produce the tag already deployed.
func Run(ctx context.Context, apps []config.App, syncFn SyncFunc, opts Options) error {
	opts.applyDefaults()
	log := opts.Log
	renderer := render.New(opts.Render)

	byName := make(map[string]config.App, len(apps))
	scheduleApps := make([]schedule.App, len(apps))
	for i, a := range apps {
		if len(a.Build) > 0 && opts.Build == nil {
			return fmt.Errorf("app %s declares builds but no build function is configured", a.Name)
		}
		byName[a.Name] = a
		scheduleApps[i] = schedule.App{Name: a.Name, Needs: a.Needs}
	}

	builds := make(map[string][]buildState, len(apps))
	deriveScopes := func(app config.App) {
		states := builds[app.Name]
		for j := range states {
			scope, err := build.WatchScope(app.Build[j])
			if err != nil {
				log.Error(err, "deriving build watch scope; watching without ignore rules", "app", app.Name, "image", app.Build[j].Image)
			}
			states[j].scope = scope
		}
	}
	for _, a := range apps {
		states := make([]buildState, len(a.Build))
		for j := range states {
			states[j].dirty = true
		}
		builds[a.Name] = states
		deriveScopes(a)
	}

	// Roots are re-derived after every run because a kustomization edit can
	// change what it references (e.g. a new chartHome), and a build's
	// .dockerignore can change what affects the image.
	appRoots := make([]watch.AppRoots, len(apps))
	rebuildRoots := func(i int) {
		app := apps[i]
		roots := []string{app.Path}
		deps, err := watch.DependencyRoots(app.Path)
		if err != nil {
			log.Error(err, "deriving dependency roots; watching the app dir only", "app", app.Name)
		}
		appRoots[i] = watch.AppRoots{App: app.Name, Roots: append(roots, deps...)}
	}
	for i := range apps {
		rebuildRoots(i)
	}
	mappingEntries := func() []watch.AppRoots {
		entries := append([]watch.AppRoots{}, appRoots...)
		for _, a := range apps {
			for j, st := range builds[a.Name] {
				entries = append(entries, watch.AppRoots{
					App:    buildKey(a.Name, j),
					Roots:  st.scope.Roots,
					Ignore: st.scope.Ignored,
				})
			}
		}
		return entries
	}
	watchRoots := func() []watch.Root {
		var roots []watch.Root
		for _, ar := range appRoots {
			for _, r := range ar.Roots {
				roots = append(roots, watch.Root{Path: r})
			}
		}
		for _, a := range apps {
			for _, st := range builds[a.Name] {
				for _, r := range st.scope.Roots {
					roots = append(roots, watch.Root{Path: r, Skip: st.scope.SkipDir})
				}
			}
		}
		return roots
	}
	mapping := watch.NewMapping(mappingEntries())

	watcher, err := watch.NewWatcher(watchRoots())
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	sched := schedule.New(schedule.Options{
		Debounce:    opts.Debounce,
		MaxParallel: opts.MaxParallel,
		RetryBase:   opts.RetryBase,
		RetryMax:    opts.RetryMax,
	}, scheduleApps)
	now := time.Now()
	for _, a := range apps {
		sched.MarkDirty(a.Name, now)
	}

	results := make(chan result)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		for _, name := range sched.StartDue(time.Now()) {
			app := byName[name]
			states := builds[name]
			// Snapshot the work under the loop goroutine: the run goroutine
			// must not touch shared state.
			var todo []int
			tags := make(map[int]string, len(states))
			for j := range states {
				if states[j].dirty {
					todo = append(todo, j)
					states[j].dirty = false
				}
				if states[j].tag != "" {
					tags[j] = states[j].tag
				}
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := runApp(ctx, renderer, app, todo, tags, opts.Build, syncFn, log)
				select {
				case results <- r:
				case <-ctx.Done():
				}
			}()
		}

		var timerC <-chan time.Time
		if deadline, ok := sched.NextDeadline(); ok {
			timerC = time.After(time.Until(deadline))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-opts.Resync:
			now := time.Now()
			for _, a := range apps {
				sched.MarkDirty(a.Name, now)
			}
			log.Info("manual resync requested", "apps", len(apps))
		case path, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			now := time.Now()
			for _, key := range mapping.AffectedBy(path) {
				name, entry, isBuild := parseKey(key)
				if isBuild {
					log.V(1).Info("source change detected", "path", path, "app", name, "image", byName[name].Build[entry].Image)
					builds[name][entry].dirty = true
				} else {
					log.V(1).Info("change detected", "path", path, "app", name)
				}
				sched.MarkDirty(name, now)
			}
		case err := <-watcher.Errors:
			log.Error(err, "watch error")
		case r := <-results:
			states := builds[r.app]
			// Tags from successful builds stick even when the run failed
			// later (a failed sync must not force a rebuild); failed builds
			// re-dirty so the retry runs them again.
			for j, tag := range r.built {
				states[j].tag = tag
			}
			for _, j := range r.failed {
				states[j].dirty = true
			}
			sched.Finish(r.app, r.ok, time.Now())
			// The run may have changed what the app references.
			for i, a := range apps {
				if a.Name == r.app {
					rebuildRoots(i)
					deriveScopes(a)
				}
			}
			mapping = watch.NewMapping(mappingEntries())
			if err := watcher.SetRoots(watchRoots()); err != nil {
				log.Error(err, "re-deriving watch roots")
			}
		case <-timerC:
			// A debounce or retry deadline passed; StartDue above picks it up.
		}
	}
}

type result struct {
	app    string
	ok     bool
	built  map[int]string // entry -> dev tag, recorded even when the run fails later
	failed []int          // entries whose build must run again
}

// unbuilt returns the todo entries that built does not record — what a build
// failure must re-dirty so the scheduler retries them.
func unbuilt(todo []int, built map[int]string) []int {
	var failed []int
	for _, j := range todo {
		if _, ok := built[j]; !ok {
			failed = append(failed, j)
		}
	}
	return failed
}

// runApp is one scheduled run of one app: build the dirty entries, render,
// inject the known dev tags, sync. A build failure aborts before render —
// syncing manifests whose images were never built would deploy whatever tag
// the manifests pin, which is exactly not the local source.
func runApp(ctx context.Context, renderer *render.Renderer, app config.App, todo []int, tags map[int]string, buildFn BuildFunc, syncFn SyncFunc, log logr.Logger) result {
	started := time.Now()
	r := result{app: app.Name, built: map[int]string{}}
	for _, batch := range app.BuildBatches(todo) {
		builds := make([]config.Build, len(batch))
		for k, j := range batch {
			builds[k] = app.Build[j]
		}
		// Build progress and failures are reported by the injected BuildFunc
		// (ui.Activity): a single live line, full log only on failure. Logging
		// build start/end here too would duplicate that.
		refs, err := buildFn(ctx, builds)
		if err != nil {
			// Re-dirty everything not yet built this run: the failed batch and
			// any later batches. Successful earlier batches keep their tags.
			r.failed = unbuilt(todo, r.built)
			return r
		}
		for k, j := range batch {
			tag := build.Tag(refs[k])
			r.built[j] = tag
			tags[j] = tag
		}
	}

	res, err := renderer.Render(app.Path)
	if err != nil {
		log.Error(err, "render failed", "app", app.Name)
		return r
	}
	if len(tags) > 0 {
		images := make([]render.Image, 0, len(tags))
		for j := range app.Build {
			if tag, ok := tags[j]; ok {
				images = append(images, render.Image{Name: app.Build[j].Image, NewTag: tag})
			}
		}
		if err := res.SetImages(images); err != nil {
			log.Error(err, "injecting built image tags failed", "app", app.Name)
			return r
		}
	}
	stats, err := syncFn(ctx, app.Name, res.Objects)
	if err != nil {
		log.Error(err, "sync failed", "app", app.Name)
		return r
	}
	// Concise, consistent with `ksync sync`'s summary: app, what actually
	// changed (applied=0 on a no-op), and a human-rounded duration. pruned and
	// failed are shown only when nonzero so the common line stays short.
	kv := []any{"app", app.Name, "applied", stats.Applied}
	if stats.Pruned > 0 {
		kv = append(kv, "pruned", stats.Pruned)
	}
	if stats.Failed > 0 {
		kv = append(kv, "failed", stats.Failed)
	}
	if stats.Degraded > 0 {
		kv = append(kv, "degraded", stats.Degraded)
	}
	kv = append(kv, "took", ui.Duration(time.Since(started)))
	log.Info("synced", kv...)
	r.ok = true
	return r
}

// keySep joins app name and build-entry index into one mapping key; NUL can
// never appear in an app name (names are Kubernetes label values).
const keySep = "\x00"

func buildKey(app string, entry int) string {
	return app + keySep + strconv.Itoa(entry)
}

func parseKey(key string) (app string, entry int, isBuild bool) {
	app, num, found := strings.Cut(key, keySep)
	if !found {
		return key, 0, false
	}
	entry, _ = strconv.Atoi(num)
	return app, entry, true
}
