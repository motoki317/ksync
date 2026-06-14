package ui

import (
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/term"
)

// Progress is the live "done/total" line ksync pins to the bottom of the
// terminal during a multi-app run — the always-visible overall status, below
// the per-build lines, that updates as apps complete (vitest-style). Off a
// terminal (piped/CI) it is inert: the per-app lines and the final summary
// carry the same information there, so nothing is lost.
type Progress struct {
	track *track
	total int

	mu   sync.Mutex
	done int
}

// StartProgress pins a progress footer for a run of total items, labeled label
// (e.g. "syncing"). Off a terminal it returns an inert Progress whose methods
// are no-ops.
func StartProgress(w io.Writer, c Colors, label string, total int) *Progress {
	return startProgress(w, c, label, total, time.Now)
}

func startProgress(w io.Writer, c Colors, label string, total int, now func() time.Time) *Progress {
	p := &Progress{total: total}
	f, ok := w.(*os.File)
	isTTY := ok && c.Enabled() && term.IsTerminal(int(f.Fd()))
	if !isTTY {
		return p
	}
	fd := int(f.Fd())
	p.track = &track{
		label:  label,
		colors: c,
		start:  now(),
		now:    now,
		cols:   func() int { return cols(fd) },
	}
	p.refresh()
	liveTerm.setFooter(w, p.track)
	return p
}

// Done records one finished item and refreshes the line ("12/18").
func (p *Progress) Done() {
	if p == nil || p.track == nil {
		return
	}
	p.mu.Lock()
	p.done++
	p.mu.Unlock()
	p.refresh()
}

func (p *Progress) refresh() {
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	p.track.setTail(strconv.Itoa(done) + "/" + strconv.Itoa(p.total))
}

// Stop removes the progress line. Safe to call on an inert Progress.
func (p *Progress) Stop() {
	if p == nil || p.track == nil {
		return
	}
	liveTerm.clearFooter("")
}
