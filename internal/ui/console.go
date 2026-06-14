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

type console struct {
	mu     sync.Mutex
	w      io.Writer // the terminal the live block renders to; set by the first track
	tracks []*track  // active build/import lines, top-to-bottom in start order
	footer *track    // optional overall-progress line, pinned below the tracks
	shown  int       // how many block lines are currently on screen
	frame  int       // spinner frame index, advanced by the ticker
	ticker *time.Ticker
	stop   chan struct{}
}

// blockEmpty reports whether nothing is being rendered (no tracks, no footer);
// the ticker runs exactly while the block is non-empty. Caller holds mu.
func (c *console) blockEmpty() bool { return len(c.tracks) == 0 && c.footer == nil }

// track is one live progress line, owned by an Activity. Its tail (the latest
// line of the command's output) is updated as the command runs; liveTerm's
// ticker reads it and repaints. cols reports the current terminal width so the
// line is truncated to a single row (multi-row wrap would break the block's
// line accounting).
type track struct {
	label  string
	colors Colors
	start  time.Time
	now    func() time.Time
	cols   func() int

	mu   sync.Mutex
	tail string
}

func (t *track) setTail(s string) {
	t.mu.Lock()
	t.tail = s
	t.mu.Unlock()
}

// render is the track's current single-line content for the given spinner frame.
func (t *track) render(frame rune) string {
	t.mu.Lock()
	tail := t.tail
	t.mu.Unlock()

	meta := Duration(t.now().Sub(t.start))
	if tail != "" {
		meta = tail + "  " + meta
	}
	// Only the meta tail is truncated; the spinner+label prefix is short and
	// always shown. Width math uses plain rune counts (no color codes yet).
	prefix := string(frame) + " " + t.label + "  "
	if room := t.cols() - len([]rune(prefix)); room > 0 {
		meta = truncateRunes(meta, room)
	}
	return fmt.Sprintf("%s %s  %s", t.colors.Cyan(string(frame)), t.colors.Bold(t.label), t.colors.Dim(meta))
}

// addTrack registers a live line and (re)paints the block. The first track to
// arrive fixes the output stream and starts the animation ticker.
func (c *console) addTrack(w io.Writer, t *track) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.w == nil {
		c.w = w
	}
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

// setFooter pins t as the overall-progress line below the build tracks, or
// replaces the current one. The footer keeps the block (and the ticker) alive
// on its own, so it stays visible after the last build finishes.
func (c *console) setFooter(w io.Writer, t *track) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.w == nil {
		c.w = w
	}
	c.eraseBlock()
	c.footer = t
	c.drawBlock()
	c.ensureTicker()
}

// clearFooter removes the progress line, printing doneLine above whatever
// remains (usually nothing). The ticker stops once the block is empty.
func (c *console) clearFooter(doneLine string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eraseBlock()
	if doneLine != "" {
		_, _ = io.WriteString(c.w, doneLine)
	}
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
	lines := c.tracks
	if c.footer != nil {
		lines = append(append([]*track(nil), c.tracks...), c.footer)
	}
	for i, t := range lines {
		if i > 0 {
			_, _ = io.WriteString(c.w, "\n")
		}
		_, _ = io.WriteString(c.w, t.render(frame))
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
