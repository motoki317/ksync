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
// bulk command); the loop forms the batches via App.BuildBatches. app is the
// owning app's name — a group can be built per-app (each app owns a subset of
// its images), so it is what distinguishes the two apps' progress lines.
type BuildFunc func(ctx context.Context, app string, builds []config.Build) ([]string, error)

// Options tune the loop; zero values get sensible watch-mode defaults.
type Options struct {
	Debounce    time.Duration // default 200ms
	MaxParallel int           // 0 = no limit (matches schedule.Options)
	RetryBase   time.Duration // default 1s
	RetryMax    time.Duration // default 2m
	Render      render.Options
	Build       BuildFunc // required when any app declares builds
	Log         logr.Logger
	// Report renders one app's completed sync. The loop calls it on success so
	// the watch output matches `ksync sync`'s ship-emoji apply line rather than
	// a plain log record; when nil, the loop logs a structured "synced" line.
	Report func(app string, stats SyncStats, took time.Duration)
	// OnError is called once when an app's run ends in failure (build, render, or
	// sync) and Report therefore does not fire — the command layer uses it to
	// tear down that app's live progress group so a retry starts clean. Optional.
	OnError func(app string, err error)
	// Resync, when a value is received, marks every app dirty — the manual
	// "redeploy everything now" the watch command wires to keyboard input.
	// A nil channel simply never fires.
	Resync <-chan struct{}
}

func (o *Options) applyDefaults() {
	if o.Debounce <= 0 {
		o.Debounce = 200 * time.Millisecond
	}
	// MaxParallel is passed through untouched: the scheduler treats 0 (or
	// negative) as no cap, so the CLI default (CPU cores) and an explicit
	// --max-parallel 0 both reach it intact.
	if o.RetryBase <= 0 {
		o.RetryBase = time.Second
	}
	if o.RetryMax <= 0 {
		o.RetryMax = 2 * time.Minute
	}
	if o.Log.GetSink() == nil {
		o.Log = logr.Discard()
	}
	if o.OnError == nil {
		o.OnError = func(string, error) {}
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

	rn := &runner{
		renderer:    renderer,
		buildFn:     opts.Build,
		syncFn:      syncFn,
		report:      opts.Report,
		onError:     opts.OnError,
		maxParallel: opts.MaxParallel,
		log:         log,
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
				r := rn.run(ctx, app, todo, tags)
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

// runner holds the per-run collaborators so one scheduled app run does not
// thread half a dozen parameters; the loop builds it once and reuses it.
type runner struct {
	renderer    *render.Renderer
	buildFn     BuildFunc
	syncFn      SyncFunc
	report      func(app string, stats SyncStats, took time.Duration)
	onError     func(app string, err error)
	maxParallel int
	log         logr.Logger
}

// run is one scheduled run of one app: build the dirty entries, render, inject
// the known dev tags, sync. A build failure aborts before render — syncing
// manifests whose images were never built would deploy whatever tag the
// manifests pin, which is exactly not the local source.
func (rn *runner) run(ctx context.Context, app config.App, todo []int, tags map[int]string) result {
	started := time.Now()
	r := result{app: app.Name, built: map[int]string{}}
	built, err := BuildAll(ctx, app, todo, rn.buildFn, rn.maxParallel)
	// Successful batches keep their tags even on a partial failure; only the
	// rest re-dirty (a failed sync later must not re-trigger a good build).
	for j, tag := range built {
		r.built[j] = tag
		tags[j] = tag
	}
	if err != nil {
		r.failed = unbuilt(todo, r.built)
		rn.onError(app.Name, err)
		return r
	}

	res, err := rn.renderer.Render(app.Path)
	if err != nil {
		rn.log.Error(err, "render failed", "app", app.Name)
		rn.onError(app.Name, err)
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
			rn.log.Error(err, "injecting built image tags failed", "app", app.Name)
			rn.onError(app.Name, err)
			return r
		}
	}
	stats, err := rn.syncFn(ctx, app.Name, res.Objects)
	if err != nil {
		rn.log.Error(err, "sync failed", "app", app.Name)
		rn.onError(app.Name, err)
		return r
	}
	rn.reportSync(app.Name, stats, time.Since(started))
	r.ok = true
	return r
}

// reportSync emits one app's completed-sync line: the injected ship-emoji
// renderer when set (matching `ksync sync`'s apply line), else a structured
// log record. applied=0 on a no-op; pruned/failed/degraded show only when
// nonzero so the common line stays short.
func (rn *runner) reportSync(app string, stats SyncStats, took time.Duration) {
	if rn.report != nil {
		rn.report(app, stats, took)
		return
	}
	kv := []any{"app", app, "applied", stats.Applied}
	if stats.Pruned > 0 {
		kv = append(kv, "pruned", stats.Pruned)
	}
	if stats.Failed > 0 {
		kv = append(kv, "failed", stats.Failed)
	}
	if stats.Degraded > 0 {
		kv = append(kv, "degraded", stats.Degraded)
	}
	kv = append(kv, "took", ui.Duration(took))
	rn.log.Info("synced", kv...)
}

// BuildAll builds every batch of app's dirty entries (todo), up to maxParallel
// batches at once (<=0 means no limit), and returns each built entry's dev tag.
// A batch — one bulk group command or one ungrouped entry — is independent of
// the others, so they build concurrently: a single multi-image app's builds no
// longer run one at a time, which is the bulk of its change→applied latency.
// On the first batch failure it cancels the rest and returns the tags built so
// far plus that error, so the caller re-dirties only what did not build. Build
// progress and failures are surfaced by the injected BuildFunc (a build/import
// row per batch in the app's pipeline, full log only on failure).
func BuildAll(ctx context.Context, app config.App, todo []int, buildFn BuildFunc, maxParallel int) (map[int]string, error) {
	batches := app.BuildBatches(todo)
	built := make(map[int]string, len(todo))
	if len(batches) == 0 {
		return built, nil
	}
	var sem chan struct{}
	if maxParallel > 0 {
		sem = make(chan struct{}, maxParallel)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for _, batch := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sem != nil {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
			}
			builds := make([]config.Build, len(batch))
			for k, j := range batch {
				builds[k] = app.Build[j]
			}
			refs, err := buildFn(ctx, app.Name, builds)
			if err != nil {
				errOnce.Do(func() { firstErr = err; cancel() })
				return
			}
			mu.Lock()
			for k, j := range batch {
				built[j] = build.Tag(refs[k])
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return built, firstErr
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
