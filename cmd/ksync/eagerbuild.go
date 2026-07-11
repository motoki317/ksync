package main

import (
	"context"
	"sync"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/loop"
	"github.com/motoki317/ksync/internal/render"
)

// buildOutcome is one app's eager build result, published once its build
// goroutine finishes. tags is nil when err is set.
type buildOutcome struct {
	tags map[int]string
	err  error
}

// startEagerBuilds launches one build goroutine per build-app immediately,
// bounded by maxParallel and independent of the needs DAG — an image is local
// (docker build + load into the cluster store), so it can build while the apps
// it depends on are still deploying, instead of waiting behind them. Entries
// whose image is in overrides are skipped (the supplied ref is injected at
// deploy, not built). It returns `await`, which blocks for one app's build
// outcome (the deploy phase calls it so an unbuilt image is never applied; an
// app with nothing to build returns a zero outcome at once), and `wait`, which
// drains the goroutines on shutdown.
func startEagerBuilds(ctx context.Context, apps []config.App, buildFn loop.BuildFunc, maxParallel int, overrides map[string]render.Image) (await func(string) buildOutcome, wait func()) {
	done := make(map[string]chan struct{}, len(apps))
	outcomes := make(map[string]buildOutcome, len(apps))
	var mu sync.Mutex
	var sem chan struct{}
	if maxParallel > 0 {
		sem = make(chan struct{}, maxParallel)
	}
	var wg sync.WaitGroup
	for _, a := range apps {
		var todo []int
		for j := range a.Build {
			if _, ov := overrides[a.Build[j].Image]; !ov {
				todo = append(todo, j)
			}
		}
		if len(todo) == 0 {
			continue // build-less, or every image overridden
		}
		ch := make(chan struct{})
		done[a.Name] = ch
		app := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(ch)
			if sem != nil {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					mu.Lock()
					outcomes[app.Name] = buildOutcome{err: ctx.Err()}
					mu.Unlock()
					return
				}
				defer func() { <-sem }()
			}
			// Independent build batches within the app run concurrently too (the
			// same fan-out the watch loop uses), bounded by maxParallel.
			tags, err := loop.BuildAll(ctx, app, todo, buildFn, maxParallel)
			mu.Lock()
			outcomes[app.Name] = buildOutcome{tags: tags, err: err}
			mu.Unlock()
		}()
	}
	await = func(app string) buildOutcome {
		ch, ok := done[app]
		if !ok {
			return buildOutcome{} // app has no build
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return buildOutcome{err: ctx.Err()}
		}
		mu.Lock()
		defer mu.Unlock()
		return outcomes[app]
	}
	return await, wg.Wait
}
