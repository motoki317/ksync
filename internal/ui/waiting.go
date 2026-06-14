package ui

import (
	"io"
	"os"
	"time"

	"golang.org/x/term"
)

// Waiting is a live "still working" line for an in-process wait that produces no
// streamed output of its own — specifically a sync blocked on its resources
// becoming Healthy. Unlike Activity (which tails an external command and prints
// its own ✓/✗ on Done), Waiting is purely transient: Stop removes the line
// silently, leaving the caller's own result line (the ship-emoji apply line) as
// the single record of the app. It animates only on an interactive terminal; on
// a pipe or under a process manager it is inert, so redirected output never
// gains spurious progress noise.
type Waiting struct {
	track *track // nil when not on a terminal — every method is then a no-op
}

// StartWaiting begins a live spinner line for label (e.g. "🚢 redis  waiting for
// health"). On a non-terminal writer it returns an inert handle.
func StartWaiting(w io.Writer, c Colors, label string) *Waiting {
	f, ok := w.(*os.File)
	if !ok || !c.Enabled() || !term.IsTerminal(int(f.Fd())) {
		return &Waiting{}
	}
	fd := int(f.Fd())
	now := time.Now
	t := &track{label: label, colors: c, start: now(), now: now}
	liveTerm.addTrack(w, func() int { return cols(fd) }, t)
	return &Waiting{track: t}
}

// Status updates the trailing detail of the live line (e.g. "3 not ready").
func (w *Waiting) Status(s string) {
	if w.track != nil {
		w.track.setTail(s)
	}
}

// Stop removes the live line without printing anything — the caller emits its
// own result line. Idempotent and safe on an inert handle.
func (w *Waiting) Stop() {
	if w.track != nil {
		liveTerm.finishTrack(w.track, "")
		w.track = nil
	}
}
