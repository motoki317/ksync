package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTrack builds a track with a fixed clock and width for deterministic output.
func newTrack(label string) *track {
	clk := func() time.Time { return time.Unix(0, 0) }
	return &track{label: label, colors: Colors{}, now: clk, start: clk(), cols: func() int { return 80 }}
}

// newConsole gives each test its own coordinator (the package singleton would
// leak the ticker goroutine and shared state across tests).
func newConsole(w *bytes.Buffer) *console { return &console{w: w} }

// Several concurrent builds must each get their own line — the bug this guards:
// only one build animated while the rest sat as static "stuck" lines.
func TestConsole_AllActiveTracksRendered(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	c.addTrack(&buf, newTrack("build duo"))
	c.addTrack(&buf, newTrack("build sistema"))
	c.stopTicker() // halt animation so the buffer is stable

	got := buf.String()
	if !strings.Contains(got, "build duo") || !strings.Contains(got, "build sistema") {
		t.Errorf("both active tracks should be rendered, got:\n%q", got)
	}
}

// A status line is printed above the live block: the block is erased, the line
// written, then the block repainted — so the line never glues onto a track.
func TestConsole_LinePrintsAboveBlock(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	c.addTrack(&buf, newTrack("build duo"))
	buf.Reset() // ignore the initial paint; focus on what line() emits
	c.line(&buf, "✓ postgres  0 applied\n")
	c.stopTicker()

	got := buf.String()
	// The erase precedes the status line, and the block (the track) is repainted
	// after it — so the status text is on its own row, above the live line.
	if !strings.HasPrefix(got, eraseLine) {
		t.Errorf("line() should erase the block first, got:\n%q", got)
	}
	if i := strings.Index(got, "✓ postgres"); i < 0 || strings.Index(got, "build duo") < i {
		t.Errorf("status line should be printed above the repainted block, got:\n%q", got)
	}
}

// Finishing one of several tracks prints its done line and keeps the rest.
func TestConsole_FinishPrintsDoneAndKeepsOthers(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	t1, t2 := newTrack("build duo"), newTrack("build sistema")
	c.addTrack(&buf, t1)
	c.addTrack(&buf, t2)
	buf.Reset()
	c.finishTrack(t1, "✓ build duo  (6.1s)\n")
	c.stopTicker()

	got := buf.String()
	if !strings.Contains(got, "✓ build duo  (6.1s)") {
		t.Errorf("done line missing: %q", got)
	}
	if !strings.Contains(got, "build sistema") {
		t.Errorf("remaining track should still be rendered: %q", got)
	}
}

// With no active block, line() is a plain write — no escape sequences leak into
// piped/redirected output.
func TestConsole_LinePlainWhenNoBlock(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	c.line(&buf, "✓ postgres  0 applied\n")
	if got := buf.String(); got != "✓ postgres  0 applied\n" {
		t.Errorf("plain line should not be decorated, got %q", got)
	}
}

// Concurrent track churn and status lines must not race or panic. Under -race
// this exercises the shared block state from the ticker, addTrack/finishTrack,
// and line() at once.
func TestConsole_ConcurrentChurnIsRaceFree(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := newTrack("build x")
			c.addTrack(&buf, tr)
			for j := 0; j < 50; j++ {
				c.line(&buf, "LINE\n")
			}
			c.finishTrack(tr, "✓ build x\n")
		}()
	}
	wg.Wait()
	c.mu.Lock()
	c.stopTicker()
	c.mu.Unlock()
}
