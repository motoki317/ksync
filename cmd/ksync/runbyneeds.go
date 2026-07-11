package main

import (
	"context"
	"sync"

	"github.com/motoki317/ksync/internal/config"
)

// runByNeeds runs fn for every app, up to maxParallel concurrently, starting an
// app only once every app it needs has finished. apps must be topologically
// sorted (config.SortByNeeds). It returns the first error and, on any error,
// cancels the derived context so apps not yet started are skipped.
func runByNeeds(ctx context.Context, apps []config.App, maxParallel int, fn func(context.Context, config.App) error) error {
	if len(apps) == 0 {
		return nil
	}
	// 0 (or negative) means no limit; a cap above the app count is the same as
	// the count, so the semaphore never holds more slots than can ever be used.
	if maxParallel < 1 || maxParallel > len(apps) {
		maxParallel = len(apps)
	}
	done := make(map[string]chan struct{}, len(apps))
	for _, a := range apps {
		done[a.Name] = make(chan struct{})
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error
	for _, a := range apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[a.Name])
			for _, dep := range a.Needs {
				ch, ok := done[dep]
				if !ok {
					continue // need is outside this run's app set
				}
				select {
				case <-ch:
				case <-ctx.Done():
					return
				}
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			// A need's done channel closes on failure as well as success, and a
			// failed need cancels ctx before that close. The waits above use
			// select, which may take a ready closed-channel case even when
			// ctx.Done() is also ready — so without this re-check a dependent
			// could run after the need it waited on failed. cancel() happens
			// before the close, so a cancellation is always observable here.
			if ctx.Err() != nil {
				return
			}
			if err := fn(ctx, a); err != nil {
				errOnce.Do(func() { firstErr = err; cancel() })
			}
		}()
	}
	wg.Wait()
	return firstErr
}
