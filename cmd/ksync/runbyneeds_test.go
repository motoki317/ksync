package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/motoki317/ksync/internal/config"
)

// TestRunByNeeds_RespectsNeeds asserts a dependent app never starts before the
// app it needs has finished.
func TestRunByNeeds_RespectsNeeds(t *testing.T) {
	apps := []config.App{
		{Name: "base"},
		{Name: "app", Needs: []string{"base"}},
	}
	var mu sync.Mutex
	var order []string
	baseDone := false
	err := runByNeeds(context.Background(), apps, 4, func(_ context.Context, a config.App) error {
		mu.Lock()
		order = append(order, a.Name)
		if a.Name == "app" && !baseDone {
			mu.Unlock()
			t.Errorf("app started before base finished")
			return nil
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		if a.Name == "base" {
			mu.Lock()
			baseDone = true
			mu.Unlock()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 2 || order[0] != "base" {
		t.Errorf("order = %v, want base before app", order)
	}
}

// TestRunByNeeds_RunsIndependentConcurrently asserts apps with no needs overlap
// in time when maxParallel allows it.
func TestRunByNeeds_RunsIndependentConcurrently(t *testing.T) {
	apps := []config.App{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	var inFlight, peak int32
	err := runByNeeds(context.Background(), apps, 3, func(_ context.Context, _ config.App) error {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d, want >= 2 (apps did not overlap)", peak)
	}
}

// TestRunByNeeds_MaxParallelCaps asserts the semaphore bounds concurrency.
func TestRunByNeeds_MaxParallelCaps(t *testing.T) {
	apps := []config.App{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}
	var inFlight, peak int32
	_ = runByNeeds(context.Background(), apps, 2, func(_ context.Context, _ config.App) error {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return nil
	})
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want <= 2 (max-parallel not honored)", peak)
	}
}

// TestRunByNeeds_FirstErrorStopsDependents asserts a failed need cancels apps
// that depend on it, and the first error is returned.
func TestRunByNeeds_FirstErrorStopsDependents(t *testing.T) {
	apps := []config.App{
		{Name: "base"},
		{Name: "app", Needs: []string{"base"}},
	}
	var ran sync.Map
	err := runByNeeds(context.Background(), apps, 4, func(_ context.Context, a config.App) error {
		ran.Store(a.Name, true)
		if a.Name == "base" {
			return errors.New("boom")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected error from failed base")
	}
	if _, ok := ran.Load("app"); ok {
		t.Errorf("app ran despite its need failing")
	}
}
