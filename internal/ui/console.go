package ui

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// liveTerm coordinates all of ksync's human-facing terminal output. Builds and
// imports run concurrently, so several may be in flight at once; each gets its
// own animated progress line, and liveTerm renders them together as a block of
// live lines pinned to the bottom of the terminal. Any ordinary status line (a
// log record, a per-app summary) is printed *above* that block: liveTerm erases
// the block, writes the line, then repaints the block below it. One ticker
// drives the animation for every track. One process-wide instance is enough —
// all of ksync's human output goes to one terminal (stderr).
//
// This is what keeps concurrent builds legible: without it, only one build
// could own an in-place spinner and the rest sat as static "stuck" lines, and a
// summary written straight to stderr mid-spinner produced runs like
// "⠋ build rust-services  0.1s✓ attachment-store  0 applied".
//
// (Named liveTerm, not term, because golang.org/x/term already owns that name.)
var liveTerm = &console{}

// eraseLine returns the cursor to column 0 and clears to end of line.
const eraseLine = "\r\x1b[K"

// cursorUp moves the cursor up one row (column is unchanged).
const cursorUp = "\x1b[1A"

// liveMargin is how many columns the block holds back from the terminal's full
// width. East-Asian width is not perfectly predictable in every terminal — an
// emoji with a variation selector (e.g. ☸️) may render as two columns where the
// Unicode tables say one — and many terminals defer the wrap when a glyph lands
// in the final column (auto-margin). Painting strictly narrower than the screen
// absorbs both so a line can never wrap.
const liveMargin = 2

type console struct {
	mu     sync.Mutex
	w      io.Writer       // the terminal the live block renders to; set on first use
	cols   func() int      // terminal width source, bound alongside w
	tracks []*track        // active build/import lines, top-to-bottom in start order
	footer func() []string // optional pinned block (the live run summary), rendered below the tracks
	shown  int             // how many block lines are currently on screen
	frame  int             // spinner frame index, advanced by the ticker
	ticker *time.Ticker
	stop   chan struct{}
}

// attach binds the output stream and its width source the first time the block
// is used; later tracks/footers on the same terminal reuse them. Caller holds mu.
func (c *console) attach(w io.Writer, cols func() int) {
	if c.w == nil {
		c.w, c.cols = w, cols
	}
}

// budget is the per-line column limit: the terminal width less liveMargin (or a
// sane default off a terminal). Every painted line is clamped to it.
func (c *console) budget() int {
	w := 80
	if c.cols != nil {
		if n := c.cols(); n > 0 {
			w = n
		}
	}
	if w-liveMargin < 1 {
		return 1
	}
	return w - liveMargin
}

// blockEmpty reports whether nothing is being rendered (no tracks, no footer);
// the ticker runs exactly while the block is non-empty. Caller holds mu.
func (c *console) blockEmpty() bool { return len(c.tracks) == 0 && c.footer == nil }

// track is one live progress line, owned by an Activity. Its tail (the latest
// line of the command's output) is updated as the command runs; liveTerm's
// ticker reads it and repaints. The line is composed in full here; the console
// clamps it to the terminal width when painting (see drawBlock), so a track need
// not know the width.
type track struct {
	label  string
	colors Colors
	start  time.Time
	now    func() time.Time

	mu   sync.Mutex
	tail string
}

func (t *track) setTail(s string) {
	t.mu.Lock()
	t.tail = s
	t.mu.Unlock()
}

// render is the track's full single-line content for the given spinner frame.
// It is not width-limited here; drawBlock clamps it (tail first, since it is
// rightmost) so the label stays visible until the terminal is very narrow.
func (t *track) render(frame rune) string {
	t.mu.Lock()
	tail := t.tail
	t.mu.Unlock()

	// The running elapsed gets the same threshold color as the finished line, so
	// a stage that is taking a while warms from green toward red as you watch it.
	// The tail (latest output line) stays dim — it is context, not the headline.
	meta := Elapsed(t.colors, t.now().Sub(t.start))
	if tail != "" {
		meta = t.colors.Dim(tail) + "  " + meta
	}
	return fmt.Sprintf("%s %s  %s", t.colors.Cyan(string(frame)), t.colors.Bold(t.label), meta)
}

// addTrack registers a live line and (re)paints the block. The first track to
// arrive fixes the output stream and width source and starts the animation
// ticker.
func (c *console) addTrack(w io.Writer, cols func() int, t *track) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attach(w, cols)
	c.eraseBlock()
	c.tracks = append(c.tracks, t)
	c.drawBlock()
	c.ensureTicker()
}

// finishTrack removes a live line, printing its done summary above whatever
// tracks remain. The ticker stops once the last track is gone.
func (c *console) finishTrack(t *track, doneLine string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eraseBlock()
	if doneLine != "" {
		_, _ = io.WriteString(c.w, doneLine)
	}
	for i, x := range c.tracks {
		if x == t {
			c.tracks = append(c.tracks[:i], c.tracks[i+1:]...)
			break
		}
	}
	c.drawBlock()
	if c.blockEmpty() {
		c.stopTicker()
	}
}

// setFooter pins render's lines below the build tracks (the live run summary),
// re-rendered on every tick. It keeps the block (and the ticker) alive on its
// own, so the summary stays visible — and updating — after the last build
// finishes. render is called from the ticker goroutine, so it must be safe to
// call concurrently with the caller's own state updates.
func (c *console) setFooter(w io.Writer, cols func() int, render func() []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attach(w, cols)
	c.eraseBlock()
	c.footer = render
	c.drawBlock()
	c.ensureTicker()
}

// clearFooter removes the pinned summary; the caller prints the final summary
// itself (so it also lands in piped/CI output, where no footer ever ran). The
// ticker stops once the block is empty.
func (c *console) clearFooter() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eraseBlock()
	c.footer = nil
	c.drawBlock()
	if c.blockEmpty() {
		c.stopTicker()
	}
}

// line writes one complete status line (s includes its trailing newline) above
// the live block: erase the block, write the line, repaint the block. With no
// block active it is a plain write — so piped/redirected output stays clean.
func (c *console) line(w io.Writer, s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blockEmpty() {
		_, _ = io.WriteString(w, s)
		return
	}
	c.eraseBlock()
	_, _ = io.WriteString(c.w, s)
	c.drawBlock()
}

// eraseBlock clears the on-screen block, leaving the cursor at column 0 of where
// the block's top line was. Caller holds mu.
func (c *console) eraseBlock() {
	if c.shown == 0 {
		return
	}
	_, _ = io.WriteString(c.w, eraseLine) // clear the (cursor's) last line
	for i := 1; i < c.shown; i++ {
		_, _ = io.WriteString(c.w, cursorUp+eraseLine)
	}
	c.shown = 0
}

// drawBlock paints the active tracks top-to-bottom, leaving the cursor at the
// end of the last line (no trailing newline, so the block stays in place).
// Caller holds mu and has just erased any prior block.
func (c *console) drawBlock() {
	frame := spinnerFrames[c.frame%len(spinnerFrames)]
	lines := make([]string, 0, len(c.tracks)+1)
	for _, t := range c.tracks {
		lines = append(lines, t.render(frame))
	}
	if c.footer != nil {
		lines = append(lines, c.footer()...)
	}
	budget := c.budget()
	for i, s := range lines {
		if i > 0 {
			_, _ = io.WriteString(c.w, "\n")
		}
		_, _ = io.WriteString(c.w, clampANSI(s, budget))
	}
	c.shown = len(lines)
}

func (c *console) ensureTicker() {
	if c.ticker != nil {
		return
	}
	c.ticker = time.NewTicker(100 * time.Millisecond)
	c.stop = make(chan struct{})
	// Pass the ticker/stop in so the goroutine holds its own references even
	// after stopTicker nils the fields for the next run.
	go c.animate(c.ticker, c.stop)
}

func (c *console) stopTicker() {
	if c.ticker == nil {
		return
	}
	c.ticker.Stop()
	close(c.stop)
	c.ticker, c.stop = nil, nil
}

func (c *console) animate(tk *time.Ticker, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			c.mu.Lock()
			c.frame++
			c.eraseBlock()
			c.drawBlock()
			c.mu.Unlock()
		}
	}
}

// WriteLine writes one complete line to w (newline included), coordinated with
// any in-flight build/import progress so status lines print cleanly above the
// live block. The command layer uses it for the per-app and run-summary lines
// it prints while builds may be animating.
func WriteLine(w io.Writer, s string) { liveTerm.line(w, s) }
