package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/engine"
)

// destroyApps must sync each app to an empty target with Prune, AllowEmpty, and
// FailFast: without AllowEmpty the empty-render guard would refuse every destroy,
// and without FailFast a wedged delete (an RBAC-forbidden or webhook-denied
// prune) would be retried for --timeout instead of surfacing at once. It also
// reports each app and visits them in the given order.
func TestDestroyApps_PrunesEachWithAllowEmptyInOrder(t *testing.T) {
	apps := []config.App{{Name: "api-b"}, {Name: "db"}} // caller-supplied deletion order
	var order []string
	var opts []engine.SyncOptions
	sync := func(_ context.Context, app string, o engine.SyncOptions) ([]common.ResourceSyncResult, error) {
		order = append(order, app)
		opts = append(opts, o)
		return nil, nil
	}
	var reported []string
	report := func(app string, _ []common.ResourceSyncResult, _ time.Duration) {
		reported = append(reported, app)
	}
	if err := destroyApps(context.Background(), apps, 0, sync, report); err != nil {
		t.Fatalf("destroyApps: %v", err)
	}
	for i, o := range opts {
		if !o.Prune || !o.AllowEmpty || !o.FailFast {
			t.Errorf("app %s: SyncOptions = %+v, want Prune && AllowEmpty && FailFast", order[i], o)
		}
	}
	if got := strings.Join(order, ","); got != "api-b,db" {
		t.Errorf("destroy order = %q, want api-b,db", got)
	}
	if got := strings.Join(reported, ","); got != "api-b,db" {
		t.Errorf("reported = %q, want every app reported", got)
	}
}

// A failed delete stops the run and names the app, so a wedged teardown is not
// masked by continuing to the next app.
func TestDestroyApps_StopsAndNamesAppOnError(t *testing.T) {
	apps := []config.App{{Name: "api-b"}, {Name: "db"}}
	calls := 0
	sync := func(_ context.Context, _ string, _ engine.SyncOptions) ([]common.ResourceSyncResult, error) {
		calls++
		return nil, errors.New("boom")
	}
	err := destroyApps(context.Background(), apps, 0, sync, func(string, []common.ResourceSyncResult, time.Duration) {})
	if err == nil {
		t.Fatal("destroyApps succeeded, want an error")
	}
	if calls != 1 {
		t.Errorf("sync called %d times, want 1 (stop on first failure)", calls)
	}
	if !strings.Contains(err.Error(), "api-b") {
		t.Errorf("error %q should name the failing app", err)
	}
}
