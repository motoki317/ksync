package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/ui"
)

func runDestroy(path, kctx *string, yes *bool, timeout *time.Duration, names []string) error {
	cfg, err := config.Load(*path)
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
	if !*yes {
		return fmt.Errorf("destroy deletes every tracked resource of: %s on context %s — re-run with --yes to confirm", strings.Join(appNames(apps), ", "), kubeContext)
	}
	// Always echo the scope: a bare `destroy --yes` (no app names) deletes every
	// app, so the user must see what is about to go and on which cluster.
	fmt.Fprintf(os.Stderr, "destroying %d app(s) on context %s: %s\n", len(apps), kubeContext, strings.Join(appNames(apps), ", "))
	// Dependents go down before their dependencies.
	apps = config.SortByNeeds(apps)
	slices.Reverse(apps)

	ctx, stop := signalContext()
	defer stop()

	_, engineLog := setupLogging(false)
	eng, err := engine.New(kubeContext, engineLog)
	if err != nil {
		return err
	}
	defer eng.Close()

	out := ui.NewColors(os.Stderr)
	nameW := nameColWidth(apps)
	return destroyApps(ctx, apps, *timeout,
		func(ctx context.Context, app string, opts engine.SyncOptions) ([]common.ResourceSyncResult, error) {
			return eng.Sync(ctx, app, nil, opts)
		},
		func(app string, results []common.ResourceSyncResult, took time.Duration) {
			printSummary(os.Stderr, out, app, results, nil, took, nameW)
		})
}

// destroySyncFunc is the slice of engine.Sync that destroyApps needs — narrowed
// so the destroy flow is testable without a live cluster.
type destroySyncFunc func(ctx context.Context, app string, opts engine.SyncOptions) ([]common.ResourceSyncResult, error)

// destroyApps deletes each app's tracked resources by syncing it to an empty
// target with prune, in the given order (the caller reverses needs order so a
// dependent is removed before what it depends on). AllowEmpty is required: the
// empty target is intentional here and would otherwise trip the empty-render
// guard. FailFast keeps it stopping at the first failure so a wedged delete (an
// RBAC-forbidden or webhook-denied prune) is not masked by the convergence retry
// that `sync`/`watch` use — surfacing a stuck delete at once is the point.
func destroyApps(ctx context.Context, apps []config.App, timeout time.Duration, sync destroySyncFunc, report func(app string, results []common.ResourceSyncResult, took time.Duration)) error {
	for _, app := range apps {
		syncCtx, cancel := withTimeout(ctx, timeout)
		start := time.Now()
		results, err := sync(syncCtx, app.Name, engine.SyncOptions{Prune: true, AllowEmpty: true, FailFast: true})
		cancel()
		if err != nil {
			return fmt.Errorf("app %s: destroy: %w", app.Name, err)
		}
		report(app.Name, results, time.Since(start))
	}
	return nil
}
