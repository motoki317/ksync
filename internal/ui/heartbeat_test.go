package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// HeartbeatLine groups the running stages by kind (build before deploy), names a
// build by its owning app and image label, a deploy by its app, and sorts apps
// within a kind — so the line is stable and every reference is app-qualified.
func TestHeartbeatLine(t *testing.T) {
	c := Colors{}
	if got := HeartbeatLine(c, nil); got != "" {
		t.Errorf("no running stages should yield an empty line, got %q", got)
	}

	running := []RunningStage{
		{App: "db", Phase: phaseDeploy, Elapsed: 5 * time.Second, Tail: "waiting for health  3 not ready: a, b"},
		{App: "shop", Phase: phaseBuild, Label: "ui", Elapsed: 24 * time.Second},
		{App: "api", Phase: phaseBuild, Label: "server", Elapsed: 8 * time.Second},
	}
	line := HeartbeatLine(c, running)
	for _, want := range []string{"⏳", IconBuild, "api/server", "shop/ui", IconDeploy, "db"} {
		if !strings.Contains(line, want) {
			t.Errorf("heartbeat line should contain %q, got %q", want, line)
		}
	}
	if strings.Index(line, "api/server") > strings.Index(line, "shop/ui") {
		t.Errorf("builds should sort by app within the kind, got %q", line)
	}
	if strings.Index(line, IconBuild) > strings.Index(line, IconDeploy) {
		t.Errorf("the build group should precede the deploy group, got %q", line)
	}
	// A waiting deploy's tail is shown parenthesized, so a stalled deploy names what
	// it waits on and the tail's own commas never read as item separators.
	if !strings.Contains(line, "(waiting for health  3 not ready: a, b)") {
		t.Errorf("a waiting deploy should carry its parenthesized health-gate tail, got %q", line)
	}
}

// syncBuf is an io.Writer safe to read from the test goroutine while the heartbeat
// goroutine writes, and that signals each write — so the coordinator tests
// synchronize on real writes instead of sleeping.
type syncBuf struct {
	mu    sync.Mutex
	b     bytes.Buffer
	wrote chan struct{}
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.b.Write(p)
	select {
	case s.wrote <- struct{}{}:
	default:
	}
	return n, err
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// resetLiveTerm clears the process-wide console's inter-section bookkeeping so a
// test that writes through it does not leave a stray leading blank for the next
// test (the Sink tests assert exact output through the same singleton). Safe to
// call only once the writer goroutine has exited — Heartbeat.Stop guarantees that.
func resetLiveTerm() {
	liveTerm.mu.Lock()
	liveTerm.lastKind, liveTerm.lastBlank = sectionNone, false
	liveTerm.mu.Unlock()
}

// Off a terminal the heartbeat writes render's line every interval until stopped.
func TestHeartbeat_EmitsOffTerminalUntilStopped(t *testing.T) {
	t.Cleanup(resetLiveTerm)
	sb := &syncBuf{wrote: make(chan struct{}, 8)}
	h := StartHeartbeat(sb, Colors{}, time.Millisecond, func() string { return "tick" })
	select {
	case <-sb.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not write off a terminal")
	}
	h.Stop()
	if !strings.Contains(sb.String(), "tick") {
		t.Errorf("heartbeat should write render's line, got %q", sb.String())
	}
}

// A render that returns empty (no work in flight) prints nothing, so an idle watch
// loop stays silent between batches even as the ticker keeps firing.
func TestHeartbeat_SilentWhenRenderEmpty(t *testing.T) {
	sb := &syncBuf{wrote: make(chan struct{}, 8)}
	ticked := make(chan struct{}, 64)
	h := StartHeartbeat(sb, Colors{}, time.Millisecond, func() string {
		select {
		case ticked <- struct{}{}:
		default:
		}
		return ""
	})
	for i := 0; i < 3; i++ {
		select {
		case <-ticked:
		case <-time.After(2 * time.Second):
			t.Fatal("heartbeat did not tick")
		}
	}
	h.Stop()
	if got := sb.String(); got != "" {
		t.Errorf("an idle heartbeat (render returns empty) should write nothing, got %q", got)
	}
}

// On a terminal the heartbeat is inert — the live block already animates — so it
// never writes regardless of what render would return. A *bytes.Buffer is not a
// terminal, so isLive is exercised via a colored Colors that still fails the
// *os.File check; here the simplest assertion is that a nil render yields an inert
// heartbeat whose Stop is safe.
func TestHeartbeat_InertWhenNoRender(t *testing.T) {
	var buf bytes.Buffer
	h := StartHeartbeat(&buf, Colors{}, time.Millisecond, nil)
	h.Stop() // must be safe on an inert heartbeat
	if buf.String() != "" {
		t.Errorf("an inert heartbeat should write nothing, got %q", buf.String())
	}
}
