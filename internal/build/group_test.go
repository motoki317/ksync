package build

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// peakConcurrency launches n calls to gate.Run, each fn signalling entry then
// blocking on a shared release, and reports how many fn bodies were in flight at
// once. A barrier makes the result deterministic: it counts entries until all n
// are in (full overlap) or no further entry arrives within a grace period
// (serialization holds the rest back). groupOf names the group per call.
func peakConcurrency(n int, gate *GroupGate, parallel bool, groupOf func(int) string) int {
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		group := groupOf(i)
		go func() {
			defer wg.Done()
			_ = gate.Run(group, parallel, func() error {
				entered <- struct{}{}
				<-release
				return nil
			})
		}()
	}
	count := 0
	for count < n {
		select {
		case <-entered:
			count++
		case <-time.After(500 * time.Millisecond):
			close(release)
			wg.Wait()
			return count
		}
	}
	close(release)
	wg.Wait()
	return count
}

func sameGroup(string) func(int) string { return func(int) string { return "g" } }

func TestGroupGate_SerializesSameGroup(t *testing.T) {
	var gate GroupGate
	if peak := peakConcurrency(6, &gate, false, sameGroup("g")); peak != 1 {
		t.Fatalf("non-parallel group ran %d invocations at once, want 1", peak)
	}
}

func TestGroupGate_ParallelAllowsOverlap(t *testing.T) {
	var gate GroupGate
	if peak := peakConcurrency(6, &gate, true, sameGroup("g")); peak != 6 {
		t.Fatalf("parallel group ran %d invocations at once, want 6", peak)
	}
}

func TestGroupGate_DifferentGroupsDoNotBlock(t *testing.T) {
	var gate GroupGate
	groupOf := func(i int) string { return fmt.Sprintf("g%d", i) }
	if peak := peakConcurrency(6, &gate, false, groupOf); peak != 6 {
		t.Fatalf("distinct groups ran %d at once, want 6 (they must not block each other)", peak)
	}
}

func TestGroupGate_EmptyGroupNotSerialized(t *testing.T) {
	var gate GroupGate
	if peak := peakConcurrency(6, &gate, false, func(int) string { return "" }); peak != 6 {
		t.Fatalf("ungrouped builds ran %d at once, want 6 (an empty group is never serialized)", peak)
	}
}

func TestGroupGate_ReturnsError(t *testing.T) {
	var gate GroupGate
	want := errors.New("boom")
	if got := gate.Run("g", false, func() error { return want }); !errors.Is(got, want) {
		t.Fatalf("Run returned %v, want %v", got, want)
	}
}
