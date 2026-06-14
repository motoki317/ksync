package ui

import (
	"io"
	"os"

	"golang.org/x/term"
)

// Footer is a multi-line block pinned to the bottom of the terminal, below any
// build lines, re-rendered from a caller-supplied function on every animation
// tick — used for the live run summary that stays visible and updates as a sync
// proceeds. Off a terminal it is inert: the caller still prints a final summary,
// so nothing is lost in pipes/CI.
type Footer struct {
	active bool
}

// StartFooter pins render's output to the bottom of the terminal and refreshes
// it ~10×/s. render returns the block's current lines (no trailing newlines); it
// is called from the animation goroutine, so it must be safe to call
// concurrently with the caller's own updates to whatever state it reads.
func StartFooter(w io.Writer, c Colors, render func() []string) *Footer {
	f := &Footer{}
	file, ok := w.(*os.File)
	isTTY := ok && c.Enabled() && term.IsTerminal(int(file.Fd()))
	if !isTTY {
		return f
	}
	f.active = true
	fd := int(file.Fd())
	liveTerm.setFooter(w, func() int { return cols(fd) }, render)
	return f
}

// Stop removes the live footer. The caller prints the final summary itself (so
// it appears in piped/CI output too, where the footer never ran). Safe to call
// on an inert Footer.
func (f *Footer) Stop() {
	if f == nil || !f.active {
		return
	}
	liveTerm.clearFooter()
}
