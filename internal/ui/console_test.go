package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// fakeItem is a blockItem rendering fixed lines, so the console's erase/redraw
// mechanics are tested independently of the pipeline that produces real lines.
// A pointer type, so finishItem's identity check (==) compares pointers rather
// than struct contents (which hold an uncomparable slice).
type fakeItem struct{ ls []string }

func (f *fakeItem) lines(rune) []string { return f.ls }

func item(lines ...string) *fakeItem { return &fakeItem{ls: lines} }

// cols80 is a fixed 80-column width source for tests.
func cols80() int { return 80 }

// bigRows is a height source large enough that the row clamp never triggers, so
// these mechanics tests see every line painted.
func bigRows() int { return 1000 }

// newConsole gives each test its own coordinator (the package singleton would
// leak the ticker goroutine and shared state across tests).
func newConsole(w *bytes.Buffer) *console {
	return &console{w: w, cols: cols80, rows: bigRows}
}

// Several concurrent groups must each render — the bug this guards: only one
// animated while the rest sat as static "stuck" lines.
func TestConsole_AllActiveItemsRendered(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	c.addItem(&buf, cols80, bigRows, item("build shop"))
	c.addItem(&buf, cols80, bigRows, item("build depot"))
	c.stopTicker() // halt animation so the buffer is stable

	got := buf.String()
	if !strings.Contains(got, "build shop") || !strings.Contains(got, "build depot") {
		t.Errorf("both active items should be rendered, got:\n%q", got)
	}
}

// A status line is printed above the live block: the block is erased, the line
// written, then the block repainted — so the line never glues onto an item.
func TestConsole_LinePrintsAboveBlock(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	c.addItem(&buf, cols80, bigRows, item("build shop"))
	buf.Reset() // ignore the initial paint; focus on what line() emits
	c.line(SectionLog, &buf, "✓ postgres  0 applied\n")
	c.stopTicker()

	got := buf.String()
	// The erase precedes the status line, and the block (the item) is repainted
	// after it — so the status text is on its own row, above the live line.
	if !strings.HasPrefix(got, eraseLine) {
		t.Errorf("line() should erase the block first, got:\n%q", got)
	}
	if i := strings.Index(got, "✓ postgres"); i < 0 || strings.Index(got, "build shop") < i {
		t.Errorf("status line should be printed above the repainted block, got:\n%q", got)
	}
}

// Finishing one of several items prints its committed line and keeps the rest.
func TestConsole_FinishPrintsDoneAndKeepsOthers(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	t1, t2 := item("build shop"), item("build depot")
	c.addItem(&buf, cols80, bigRows, t1)
	c.addItem(&buf, cols80, bigRows, t2)
	buf.Reset()
	c.finishItem(t1, "✓ build shop  (6.1s)\n")
	c.stopTicker()

	got := buf.String()
	if !strings.Contains(got, "✓ build shop  (6.1s)") {
		t.Errorf("committed line missing: %q", got)
	}
	if !strings.Contains(got, "build depot") {
		t.Errorf("remaining item should still be rendered: %q", got)
	}
}

// With no active block, line() is a plain write — no escape sequences leak into
// piped/redirected output.
func TestConsole_LinePlainWhenNoBlock(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	c.line(SectionLog, &buf, "✓ postgres  0 applied\n")
	if got := buf.String(); got != "✓ postgres  0 applied\n" {
		t.Errorf("plain line should not be decorated, got %q", got)
	}
}

// The footer (the live summary block) renders below the items and keeps the
// block alive on its own — after the last item finishes it stays visible and
// can span multiple lines.
func TestConsole_FooterRendersBelowAndPersists(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	build := item("build shop")
	c.addItem(&buf, cols80, bigRows, build)
	c.setFooter(&buf, cols80, bigRows, func() []string { return []string{"Summary", "  Apps  1/2 synced"} })
	buf.Reset()
	c.finishItem(build, "✓ build shop  (4s)\n") // last item done; footer remains
	c.stopTicker()

	got := buf.String()
	if !strings.Contains(got, "✓ build shop  (4s)") {
		t.Errorf("finished item's committed line missing: %q", got)
	}
	if !strings.Contains(got, "Summary") || !strings.Contains(got, "1/2 synced") {
		t.Errorf("multi-line footer should persist after the last item finishes: %q", got)
	}
}

// A status line erases the footer block, prints above it, and repaints it — so
// per-app summaries scroll above the pinned live summary.
func TestConsole_LineAboveFooterOnly(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	c.setFooter(&buf, cols80, bigRows, func() []string { return []string{"Summary", "  Apps  0/4 synced"} })
	buf.Reset()
	c.line(SectionLog, &buf, "✓ postgres  0 applied\n")
	c.stopTicker()

	got := buf.String()
	if !strings.HasPrefix(got, eraseLine) {
		t.Errorf("line() should erase the footer block first: %q", got)
	}
	if i := strings.Index(got, "✓ postgres"); i < 0 || strings.Index(got, "Summary") < i {
		t.Errorf("status line should print above the repainted footer: %q", got)
	}
}

// drawBlock must clamp every painted line — emoji-prefixed rows included — to
// within the terminal width, so none wraps and the cursor-up erase stays
// accurate. Guards the regression where the 🔨 icon (two columns counted as one)
// pushed lines one past the edge, wrapping them and corrupting the block.
func TestConsole_DrawBlockClampsToWidth(t *testing.T) {
	var buf bytes.Buffer
	const width = 24
	c := &console{w: &buf, cols: func() int { return width }, rows: bigRows}
	c.items = append(c.items,
		item("🔨 bundle (shop)  loading metadata for a very long image reference :nonroot"),
		item("📦 some-other-build"),
	)
	c.drawBlock()
	c.stopTicker()

	for _, ln := range strings.Split(buf.String(), "\n") {
		if w := displayWidth(ln); w > width {
			t.Errorf("painted line is %d cols, exceeds terminal width %d: %q", w, width, ln)
		}
	}
}

// When the block would exceed the terminal height, item lines are trimmed with a
// "… N more" marker while the footer is kept in full — so a screenful of
// concurrent groups can never grow the block past the screen and break the
// cursor-up erase math.
func TestConsole_ClampsToHeight(t *testing.T) {
	var buf bytes.Buffer
	// 5 rows of budget (height 6 - 1); the footer takes 3 (its separator blank,
	// Summary, the Apps row) and the marker 1, so 1 item row fits.
	c := &console{w: &buf, cols: cols80, rows: func() int { return 6 }}
	for i := 0; i < 6; i++ {
		c.items = append(c.items, item("group-"+string(rune('a'+i))))
	}
	c.setFooter(&buf, cols80, func() int { return 6 }, func() []string { return []string{"Summary", "  Apps  0/6"} })
	buf.Reset()
	c.refresh()
	c.stopTicker()

	got := buf.String()
	if strings.Count(got, "\n") != 4 { // 5 lines => 4 newlines between them
		t.Errorf("block should be clamped to 5 rows, got:\n%q", got)
	}
	if !strings.Contains(got, "… 5 more") {
		t.Errorf("elision marker should report the dropped groups, got:\n%q", got)
	}
	if !strings.Contains(got, "Summary") || !strings.Contains(got, "Apps  0/6") {
		t.Errorf("footer must be kept in full under the clamp, got:\n%q", got)
	}
}

// Concurrent item churn and status lines must not race or panic. Under -race
// this exercises the shared block state from the ticker, addItem/finishItem,
// and line() at once.
func TestConsole_ConcurrentChurnIsRaceFree(t *testing.T) {
	var buf bytes.Buffer
	c := newConsole(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it := item("build x")
			c.addItem(&buf, cols80, bigRows, it)
			for j := 0; j < 50; j++ {
				c.line(SectionLog, &buf, "LINE\n")
			}
			c.finishItem(it, "✓ build x\n")
		}()
	}
	wg.Wait()
	c.mu.Lock()
	c.stopTicker()
	c.mu.Unlock()
}
