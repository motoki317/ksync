package schedule

import (
	"reflect"
	"testing"
	"time"
)

// t0 is an arbitrary fixed instant; the scheduler only ever compares times.
var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func TestScheduler_DebouncesBurstsIntoOneRun(t *testing.T) {
	s := New(Options{Debounce: 100 * time.Millisecond}, []App{{Name: "api-b"}})
	// A burst of file events; each one slides the deadline.
	s.MarkDirty("api-b", at(0))
	s.MarkDirty("api-b", at(50*time.Millisecond))
	s.MarkDirty("api-b", at(80*time.Millisecond))

	if got := s.StartDue(at(150 * time.Millisecond)); len(got) != 0 {
		t.Errorf("StartDue before the quiet period elapsed = %v, want none", got)
	}
	if got := s.StartDue(at(180 * time.Millisecond)); !reflect.DeepEqual(got, []string{"api-b"}) {
		t.Errorf("StartDue after quiet period = %v, want [api-b]", got)
	}
	// The burst coalesced into exactly one run.
	if got := s.StartDue(at(time.Second)); len(got) != 0 {
		t.Errorf("StartDue again = %v, want none (already running)", got)
	}
}

func TestScheduler_SerializesRunsPerApp(t *testing.T) {
	s := New(Options{}, []App{{Name: "api-b"}})
	s.MarkDirty("api-b", at(0))
	if got := s.StartDue(at(0)); len(got) != 1 {
		t.Fatalf("StartDue = %v, want [api-b]", got)
	}
	// Changes arriving mid-run coalesce into one follow-up run, started only
	// after the current run finishes.
	s.MarkDirty("api-b", at(10*time.Millisecond))
	s.MarkDirty("api-b", at(20*time.Millisecond))
	if got := s.StartDue(at(time.Second)); len(got) != 0 {
		t.Fatalf("StartDue while running = %v, want none", got)
	}
	s.Finish("api-b", true, at(2*time.Second))
	if got := s.StartDue(at(2 * time.Second)); !reflect.DeepEqual(got, []string{"api-b"}) {
		t.Errorf("StartDue after finish = %v, want [api-b] (one coalesced follow-up)", got)
	}
	s.Finish("api-b", true, at(3*time.Second))
	if got := s.StartDue(at(4 * time.Second)); len(got) != 0 {
		t.Errorf("StartDue after follow-up = %v, want none", got)
	}
}

func TestScheduler_RunsIndependentAppsInParallel(t *testing.T) {
	s := New(Options{}, []App{{Name: "db"}, {Name: "api-b"}})
	s.MarkDirty("db", at(0))
	s.MarkDirty("api-b", at(0))
	if got := s.StartDue(at(0)); !reflect.DeepEqual(got, []string{"db", "api-b"}) {
		t.Errorf("StartDue = %v, want both apps", got)
	}
}

func TestScheduler_BoundsParallelism(t *testing.T) {
	s := New(Options{MaxParallel: 2}, []App{{Name: "a"}, {Name: "b"}, {Name: "c"}})
	for _, name := range []string{"a", "b", "c"} {
		s.MarkDirty(name, at(0))
	}
	if got := s.StartDue(at(0)); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("StartDue = %v, want [a b] (declaration order, capped at 2)", got)
	}
	if got := s.StartDue(at(time.Second)); len(got) != 0 {
		t.Fatalf("StartDue at cap = %v, want none", got)
	}
	s.Finish("a", true, at(2*time.Second))
	if got := s.StartDue(at(2 * time.Second)); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("StartDue after a slot freed = %v, want [c]", got)
	}
}

func TestScheduler_NeedsGateAppsWithinTheDirtySet(t *testing.T) {
	s := New(Options{}, []App{{Name: "db"}, {Name: "api-b", Needs: []string{"db"}}})
	s.MarkDirty("db", at(0))
	s.MarkDirty("api-b", at(0))
	if got := s.StartDue(at(0)); !reflect.DeepEqual(got, []string{"db"}) {
		t.Fatalf("StartDue = %v, want [db] (api-b waits for its dependency)", got)
	}
	s.Finish("db", true, at(time.Second))
	if got := s.StartDue(at(time.Second)); !reflect.DeepEqual(got, []string{"api-b"}) {
		t.Errorf("StartDue after db finished = %v, want [api-b]", got)
	}
}

func TestScheduler_NeedsDoNotGateWhenDependencyIsClean(t *testing.T) {
	s := New(Options{}, []App{{Name: "db"}, {Name: "api-b", Needs: []string{"db"}}})
	s.MarkDirty("api-b", at(0))
	if got := s.StartDue(at(0)); !reflect.DeepEqual(got, []string{"api-b"}) {
		t.Errorf("StartDue = %v, want [api-b] (db is clean, nothing to wait for)", got)
	}
}

func TestScheduler_FailedDependencyKeepsGatingUntilItSucceeds(t *testing.T) {
	s := New(Options{RetryBase: time.Second}, []App{{Name: "db"}, {Name: "api-b", Needs: []string{"db"}}})
	s.MarkDirty("db", at(0))
	s.MarkDirty("api-b", at(0))
	if got := s.StartDue(at(0)); !reflect.DeepEqual(got, []string{"db"}) {
		t.Fatalf("StartDue = %v, want [db]", got)
	}
	s.Finish("db", false, at(100*time.Millisecond)) // db failed; retry pending
	if got := s.StartDue(at(200 * time.Millisecond)); len(got) != 0 {
		t.Errorf("StartDue = %v, want none (api-b still gated, db backoff not elapsed)", got)
	}
	if got := s.StartDue(at(1100 * time.Millisecond)); !reflect.DeepEqual(got, []string{"db"}) {
		t.Fatalf("StartDue after backoff = %v, want [db] retry", got)
	}
	s.Finish("db", true, at(1200*time.Millisecond))
	if got := s.StartDue(at(1200 * time.Millisecond)); !reflect.DeepEqual(got, []string{"api-b"}) {
		t.Errorf("StartDue = %v, want [api-b] (dependency finally clean)", got)
	}
}

func TestScheduler_FailuresRetryWithExponentialBackoff(t *testing.T) {
	s := New(Options{RetryBase: time.Second, RetryMax: 3 * time.Second}, []App{{Name: "api-b"}})
	s.MarkDirty("api-b", at(0))
	s.StartDue(at(0))

	s.Finish("api-b", false, at(0)) // 1st failure: retry after 1s
	if got := s.StartDue(at(900 * time.Millisecond)); len(got) != 0 {
		t.Errorf("StartDue before backoff = %v, want none", got)
	}
	if got := s.StartDue(at(time.Second)); len(got) != 1 {
		t.Fatalf("StartDue at backoff = %v, want retry", got)
	}

	s.Finish("api-b", false, at(time.Second)) // 2nd failure: retry after 2s
	if got := s.StartDue(at(2500 * time.Millisecond)); len(got) != 0 {
		t.Errorf("StartDue before doubled backoff = %v, want none", got)
	}
	if got := s.StartDue(at(3 * time.Second)); len(got) != 1 {
		t.Fatalf("StartDue at doubled backoff = %v, want retry", got)
	}

	s.Finish("api-b", false, at(3*time.Second)) // 3rd failure: 4s capped to 3s
	if got := s.StartDue(at(5900 * time.Millisecond)); len(got) != 0 {
		t.Errorf("StartDue before capped backoff = %v, want none", got)
	}
	if got := s.StartDue(at(6 * time.Second)); len(got) != 1 {
		t.Errorf("StartDue at capped backoff = %v, want retry", got)
	}
}

func TestScheduler_NewChangeResetsBackoff(t *testing.T) {
	s := New(Options{Debounce: 100 * time.Millisecond, RetryBase: 10 * time.Second}, []App{{Name: "api-b"}})
	s.MarkDirty("api-b", at(0))
	s.StartDue(at(100 * time.Millisecond))
	s.Finish("api-b", false, at(200*time.Millisecond)) // retry would wait 10s

	// The user edits the file again (plausibly fixing the failure): the
	// debounce deadline replaces the long backoff and attempts start over.
	s.MarkDirty("api-b", at(300*time.Millisecond))
	if got := s.StartDue(at(400 * time.Millisecond)); len(got) != 1 {
		t.Fatalf("StartDue after fresh edit = %v, want immediate re-run (not backoff)", got)
	}
	s.Finish("api-b", false, at(500*time.Millisecond))
	// Backoff restarts from the base, not from the previous attempt count.
	if got := s.StartDue(at(10500*time.Millisecond + 100*time.Millisecond)); len(got) != 1 {
		t.Errorf("StartDue after base backoff = %v, want retry", got)
	}
}

func TestScheduler_NextDeadline(t *testing.T) {
	s := New(Options{Debounce: 100 * time.Millisecond}, []App{{Name: "db"}, {Name: "api-b"}})
	if _, ok := s.NextDeadline(); ok {
		t.Error("NextDeadline on an idle scheduler reported a deadline")
	}
	s.MarkDirty("api-b", at(0))
	s.MarkDirty("db", at(50*time.Millisecond))
	got, ok := s.NextDeadline()
	if !ok || !got.Equal(at(100*time.Millisecond)) {
		t.Errorf("NextDeadline = %v,%v, want %v (earliest pending deadline)", got, ok, at(100*time.Millisecond))
	}
}

func TestScheduler_IdleReportsNoPendingWork(t *testing.T) {
	s := New(Options{}, []App{{Name: "api-b"}})
	if !s.Idle() {
		t.Error("new scheduler not idle")
	}
	s.MarkDirty("api-b", at(0))
	if s.Idle() {
		t.Error("dirty scheduler reported idle")
	}
	s.StartDue(at(0))
	if s.Idle() {
		t.Error("running scheduler reported idle")
	}
	s.Finish("api-b", true, at(time.Second))
	if !s.Idle() {
		t.Error("finished scheduler not idle")
	}
}
