package ui

import (
	"io"
	"sync"
)

// liveTerm serializes every write to the live terminal so the animated
// build/import spinner and the plain status lines other goroutines print
// concurrently never land on the same physical line. While an Activity animates
// it leaves the cursor mid-line — a spinner frame with no trailing newline — so
// any other writer must erase that frame before printing its own line; the
// spinner then repaints on its next 100ms tick. One process-wide instance is
// enough: all of ksync's human output goes to one terminal (stderr).
//
// Without this, a per-app "✓ app  N applied" line written straight to stderr
// while a build spinner was on the line produced runs like
// "⠋ build rust-services  0.1s✓ attachment-store  0 applied".
//
// (Named liveTerm, not term, because golang.org/x/term already owns that name.)
var liveTerm console

type console struct {
	mu      sync.Mutex
	drawn   bool      // a spinner frame is on the current line (no newline yet)
	spinner io.Writer // the stream it was drawn to, so only that one is erased
}

// eraseLine returns the cursor to column 0 and clears to end of line.
const eraseLine = "\r\x1b[K"

// spinnerFrame repaints one in-place spinner frame on w: erase the line, write
// s, no newline. Records the line as dirty so the next line() clears it first.
func (t *console) spinnerFrame(w io.Writer, s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = io.WriteString(w, eraseLine+s)
	t.drawn, t.spinner = true, w
}

// line writes one complete line (s must include its trailing newline), first
// erasing any spinner frame on the same stream so the spinner and the line
// never collide. The spinner repaints itself on its next tick.
func (t *console) line(w io.Writer, s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.drawn && t.spinner == w {
		_, _ = io.WriteString(w, eraseLine)
		t.drawn = false
	}
	_, _ = io.WriteString(w, s)
}

// WriteLine writes one complete line to w (newline included), coordinated with
// any in-flight build/import spinner so concurrent status lines and the spinner
// never share a physical line. The command layer uses it for the per-app and
// run-summary lines it prints while builds may be animating.
func WriteLine(w io.Writer, s string) { liveTerm.line(w, s) }
