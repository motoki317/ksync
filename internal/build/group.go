package build

import "sync"

// GroupGate serializes a build group's bulk command across the concurrent
// per-app builds. ksync builds apps in parallel, so two apps that share a build
// group would otherwise invoke the group's command at the same time — and a bulk
// command need not be concurrency-safe: cargo-zigbuild, for one, lazily creates a
// shared wrapper cache and two simultaneous cold invocations race to create it,
// failing one with "File exists (os error 17)" and leaving its image unbuilt.
// By default the gate admits one invocation of a given group at a time, mirroring
// the imageLoad Loader, which serializes for the same class of reason. A group
// whose command is concurrency-safe opts out with parallel and the gate never
// blocks it.
//
// The zero value is ready to use and safe for concurrent callers. Locks are
// per-group, so different groups (and ungrouped builds) never block each other.
type GroupGate struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Run calls fn, serialized against other Run calls for the same group unless
// parallel is set. An empty group name is treated as ungrouped and is never
// serialized. fn's error is returned unchanged.
func (g *GroupGate) Run(group string, parallel bool, fn func() error) error {
	if group == "" || parallel {
		return fn()
	}
	g.lockFor(group).Lock()
	defer g.lockFor(group).Unlock()
	return fn()
}

// lockFor returns the mutex guarding group, creating it on first use. The same
// pointer is returned for a given group for the gate's lifetime, so the Lock in
// Run and the Unlock in its deferred call operate on one mutex.
func (g *GroupGate) lockFor(group string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.locks == nil {
		g.locks = make(map[string]*sync.Mutex)
	}
	m := g.locks[group]
	if m == nil {
		m = &sync.Mutex{}
		g.locks[group] = m
	}
	return m
}
