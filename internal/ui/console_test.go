package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTrack builds a track with a fixed clock for deterministic output.
func newTrack(label string) *track {
	clk := func() time.Time { return time.Unix(0, 0) }
	return &track{label: label, colors: Colors{}, now: clk, start: clk()}
}

// cols80 is a fixed 80-column width source for tests.
func cols80() int { return 80 }

// newConsole gives each test its own coordinator (the package singleton would
// leak the ticker goroutine and shared state across tests).
func newConsole(w *bytes.Buffer) *console { return &console{w: w, cols: cols80} }

// Several concurrent builds must each get their own line — the bug this guards:
// only one build animated while the rest sat as static "stuck" lines.
func TestConsole_AllActiveTracksRendered(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	c.addTrack(&buf, cols80, newTrack("build duo"))
	c.addTrack(&buf, cols80, newTrack("build sistema"))
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
	c.addTrack(&buf, cols80, newTrack("build duo"))
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
	c.addTrack(&buf, cols80, t1)
	c.addTrack(&buf, cols80, t2)
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

// The footer (the live summary block) renders below the build tracks and keeps
// the block alive on its own — after the last build finishes it stays visible
// and can span multiple lines.
func TestConsole_FooterRendersBelowAndPersists(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	build := newTrack("build duo")
	c.addTrack(&buf, cols80, build)
	c.setFooter(&buf, cols80, func() []string { return []string{"Summary", "  Apps  1/2 synced"} })
	buf.Reset()
	c.finishTrack(build, "✓ build duo  (4s)\n") // last build done; footer remains
	c.stopTicker()

	got := buf.String()
	if !strings.Contains(got, "✓ build duo  (4s)") {
		t.Errorf("finished build's done line missing: %q", got)
	}
	if !strings.Contains(got, "Summary") || !strings.Contains(got, "1/2 synced") {
		t.Errorf("multi-line footer should persist after the last build finishes: %q", got)
	}
}

// A status line erases the footer block, prints above it, and repaints it — so
// per-app summaries scroll above the pinned live summary.
func TestConsole_LineAboveFooterOnly(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	c.setFooter(&buf, cols80, func() []string { return []string{"Summary", "  Apps  0/4 synced"} })
	buf.Reset()
	c.line(&buf, "✓ postgres  0 applied\n")
	c.stopTicker()

	got := buf.String()
	if !strings.HasPrefix(got, eraseLine) {
		t.Errorf("line() should erase the footer block first: %q", got)
	}
	if i := strings.Index(got, "✓ postgres"); i < 0 || strings.Index(got, "Summary") < i {
		t.Errorf("status line should print above the repainted footer: %q", got)
	}
}

// drawBlock must clamp every painted line — emoji-prefixed track lines included —
// to within the terminal width, so none wraps and the cursor-up erase stays
// accurate. Guards the regression where the 🔨 icon (two columns counted as one)
// pushed lines one past the edge, wrapping them and corrupting the block.
func TestConsole_DrawBlockClampsToWidth(t *testing.T) {
	var buf bytes.Buffer
	const width = 24
	c := &console{w: &buf, cols: func() int { return width }}
	a := newTrack("🔨 rust-services (duo)")
	a.setTail("loading metadata for a very long image reference :nonroot")
	b := newTrack("📦 some-other-build")
	c.tracks = append(c.tracks, a, b)
	c.drawBlock()
	c.stopTicker()

	for _, ln := range strings.Split(buf.String(), "\n") {
		if w := displayWidth(ln); w > width {
			t.Errorf("painted line is %d cols, exceeds terminal width %d: %q", w, width, ln)
		}
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
			c.addTrack(&buf, cols80, tr)
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
