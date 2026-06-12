// Package loop is the watch-mode heart of ksync: it wires the file watcher,
// the dirty-set mapping, the scheduler, and the renderer into one event loop
// that renders and syncs apps as their inputs change. The cluster side is
// injected as a SyncFunc so the loop is testable without a cluster.
package loop

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
	"github.com/motoki317/ksync/internal/schedule"
	"github.com/motoki317/ksync/internal/watch"
)

// SyncFunc applies one app's rendered objects to the cluster.
type SyncFunc func(ctx context.Context, app string, objects []*unstructured.Unstructured) error

// Options tune the loop; zero values get sensible watch-mode defaults.
type Options struct {
	Debounce    time.Duration // default 200ms
	MaxParallel int           // default 4
	RetryBase   time.Duration // default 1s
	RetryMax    time.Duration // default 2m
	Render      render.Options
	Log         logr.Logger
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

// Run watches the apps' inputs and renders+syncs them on change until ctx is
// done. Every app is synced once at startup so the cluster converges to the
// current working tree before incremental behavior takes over.
func Run(ctx context.Context, apps []config.App, syncFn SyncFunc, opts Options) error {
	opts.applyDefaults()
	log := opts.Log
	renderer := render.New(opts.Render)

	byName := make(map[string]config.App, len(apps))
	scheduleApps := make([]schedule.App, len(apps))
	for i, a := range apps {
		byName[a.Name] = a
		scheduleApps[i] = schedule.App{Name: a.Name, Needs: a.Needs}
	}

	// Roots are re-derived after every run because a kustomization edit can
	// change what it references (e.g. a new chartHome).
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
	mapping := watch.NewMapping(appRoots)

	watcher, err := watch.NewWatcher(mapping.AllRoots())
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

	type result struct {
		app string
		ok  bool
	}
	results := make(chan result)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		for _, name := range sched.StartDue(time.Now()) {
			app := byName[name]
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok := renderAndSync(ctx, renderer, app, syncFn, log)
				select {
				case results <- result{app: app.Name, ok: ok}:
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
		case path, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			now := time.Now()
			for _, app := range mapping.AffectedBy(path) {
				log.V(1).Info("change detected", "path", path, "app", app)
				sched.MarkDirty(app, now)
			}
		case err := <-watcher.Errors:
			log.Error(err, "watch error")
		case r := <-results:
			sched.Finish(r.app, r.ok, time.Now())
			// The run may have changed what the app references.
			for i, a := range apps {
				if a.Name == r.app {
					rebuildRoots(i)
				}
			}
			mapping = watch.NewMapping(appRoots)
			for _, root := range mapping.AllRoots() {
				_ = watcher.Add(root) // idempotent; new roots start being watched
			}
		case <-timerC:
			// A debounce or retry deadline passed; StartDue above picks it up.
		}
	}
}

func renderAndSync(ctx context.Context, renderer *render.Renderer, app config.App, syncFn SyncFunc, log logr.Logger) bool {
	started := time.Now()
	res, err := renderer.Render(app.Path)
	if err != nil {
		log.Error(err, "render failed", "app", app.Name)
		return false
	}
	if err := syncFn(ctx, app.Name, res.Objects); err != nil {
		log.Error(err, "sync failed", "app", app.Name)
		return false
	}
	log.Info("synced", "app", app.Name, "objects", len(res.Objects), "took", time.Since(started).String())
	return true
}
