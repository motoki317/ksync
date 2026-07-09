// Package schedule decides when dirty apps run: it debounces change bursts,
// coalesces them into single runs, serializes runs per app while letting
// independent apps run in parallel (bounded), gates dependents on their
// `needs` edges, and schedules retries with exponential backoff after
// failures.
//
// The Scheduler is a pure state machine — callers feed it events
// (MarkDirty, Finish) and time, and poll StartDue/NextDeadline. It does no
// I/O and starts no goroutines, which is what makes the watch loop's timing
// behavior unit-testable; it is not safe for concurrent use (the watch loop
// drives it from one goroutine). The watch loop runs two of them — one for
// the needs-free build phase and one for the needs-gated deploy phase — so a
// deploy can additionally wait on the loop's own state (its image still
// building) via the optional external gate (SetExternalBlock).
package schedule

import "time"

// App declares one schedulable app and its dependency edges (config has
// already validated that the edges form a DAG).
type App struct {
	Name  string
	Needs []string
}

// Options shape the scheduling behavior. Zero values mean: run immediately
// (no debounce), unlimited parallelism, immediate uncapped retries.
type Options struct {
	// Debounce is the quiet period after the last change before a run starts.
	Debounce time.Duration
	// MaxParallel caps concurrently running apps; 0 means no cap.
	MaxParallel int
	// RetryBase is the delay before the first retry after a failure; it
	// doubles per consecutive failure, capped at RetryMax (when set).
	RetryBase time.Duration
	RetryMax  time.Duration
}

type appState struct {
	needs    []string
	dirty    bool
	deadline time.Time
	running  bool
	attempts int
}

// Scheduler tracks the dirty/running state of all apps.
type Scheduler struct {
	opts    Options
	order   []string // declaration order, the iteration and priority order
	apps    map[string]*appState
	running int
	// extBlock is an optional gate beyond `needs`: it reports whether an app
	// cannot start yet for a reason the scheduler does not model. The deploy
	// scheduler uses it to wait while the app's image is still building. Like
	// `blocked`, it clears on an event (a build result re-polls StartDue), not
	// on time, so deadlines of externally-blocked apps are kept out of
	// NextDeadline to avoid a hot poll loop.
	extBlock func(name string) bool
}

func New(opts Options, apps []App) *Scheduler {
	s := &Scheduler{opts: opts, apps: make(map[string]*appState, len(apps))}
	for _, a := range apps {
		s.order = append(s.order, a.Name)
		s.apps[a.Name] = &appState{needs: a.Needs}
	}
	return s
}

// SetExternalBlock installs the optional external gate (see Scheduler.extBlock).
// It is evaluated synchronously inside StartDue/NextDeadline, so the predicate
// may read state the same goroutine owns without locking. A nil fn clears it.
func (s *Scheduler) SetExternalBlock(fn func(name string) bool) { s.extBlock = fn }

// externallyBlocked reports whether the external gate currently holds name back.
func (s *Scheduler) externallyBlocked(name string) bool {
	return s.extBlock != nil && s.extBlock(name)
}

// MarkDirty records a change to app name at time now. Each call slides the
// app's run deadline to now+Debounce, so a burst of events coalesces into one
// run. It also resets the retry backoff: the content changed, so the failure
// streak no longer describes what will be run.
func (s *Scheduler) MarkDirty(name string, now time.Time) {
	st, ok := s.apps[name]
	if !ok {
		return
	}
	st.dirty = true
	st.deadline = now.Add(s.opts.Debounce)
	st.attempts = 0
}

// StartDue returns the apps whose run should start now, marking them as
// running. An app is due when it is dirty, not already running, past its
// deadline, within the parallelism cap, and none of its needs have pending
// work. Apps are considered in declaration order.
//
// Callers must report each returned app back via Finish, and should call
// StartDue again after every MarkDirty, Finish, or deadline expiry.
func (s *Scheduler) StartDue(now time.Time) []string {
	var started []string
	for _, name := range s.order {
		if s.opts.MaxParallel > 0 && s.running >= s.opts.MaxParallel {
			break
		}
		st := s.apps[name]
		if !st.dirty || st.running || now.Before(st.deadline) || s.blocked(st) || s.externallyBlocked(name) {
			continue
		}
		st.dirty = false
		st.running = true
		s.running++
		started = append(started, name)
	}
	return started
}

// blocked reports whether a dependency still has pending work; the dependent
// waits so it never syncs against a half-updated dependency.
func (s *Scheduler) blocked(st *appState) bool {
	for _, dep := range st.needs {
		if ds, ok := s.apps[dep]; ok && (ds.dirty || ds.running) {
			return true
		}
	}
	return false
}

// Finish records the completion of a run started via StartDue. A failure
// re-dirties the app with an exponential-backoff deadline — unless an edit
// during the run already scheduled a sooner one — and returns the app's new
// attempt count and that backoff delay so the caller can report the pending
// retry. A success returns (0, 0).
func (s *Scheduler) Finish(name string, ok bool, now time.Time) (attempts int, retryIn time.Duration) {
	st, found := s.apps[name]
	if !found || !st.running {
		return 0, 0
	}
	st.running = false
	s.running--
	if ok {
		st.attempts = 0
		return 0, 0
	}
	st.attempts++
	retryIn = s.retryDelay(st.attempts)
	deadline := now.Add(retryIn)
	if !st.dirty || deadline.Before(st.deadline) {
		st.deadline = deadline
	}
	st.dirty = true
	return st.attempts, retryIn
}

// hardBackoffCeiling bounds the doubling loop when RetryMax is unset, both as
// a sane upper limit and as an overflow guard.
const hardBackoffCeiling = 24 * time.Hour

func (s *Scheduler) retryDelay(attempts int) time.Duration {
	max := s.opts.RetryMax
	if max <= 0 || max > hardBackoffCeiling {
		max = hardBackoffCeiling
	}
	d := s.opts.RetryBase
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return min(d, max)
}

// NextDeadline returns the earliest deadline among apps that can actually run
// when it elapses, so the caller knows when to poll StartDue next. Running,
// needs-blocked, and externally-blocked apps are excluded: a blocked app's
// deadline is already in the past while its dependency runs, so reporting it
// would make the caller's timer fire immediately and spin until the
// dependency's Finish — which is itself the event that re-polls StartDue.
func (s *Scheduler) NextDeadline() (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, name := range s.order {
		st := s.apps[name]
		if !st.dirty || st.running || s.blocked(st) || s.externallyBlocked(name) {
			continue
		}
		if !found || st.deadline.Before(earliest) {
			earliest = st.deadline
			found = true
		}
	}
	return earliest, found
}

// Idle reports whether no app is dirty or running.
func (s *Scheduler) Idle() bool {
	if s.running > 0 {
		return false
	}
	for _, st := range s.apps {
		if st.dirty {
			return false
		}
	}
	return true
}
