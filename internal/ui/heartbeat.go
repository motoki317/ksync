package ui

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Heartbeat periodically prints a one-line liveness snapshot whenever output is not
// a live animated block, so a long build or deploy in a pipe or CI log shows
// progress instead of looking hung. It is the inverse of Footer: active exactly
// when the live block is not (so also under NO_COLOR on a real terminal, where the
// block is disabled too), inert when the block animates the work itself.
type Heartbeat struct {
	stop     chan struct{}
	done     chan struct{} // closed when the goroutine exits, so Stop can wait it out
	stopOnce sync.Once     // guards close(stop) so concurrent/repeat Stop never double-closes
}

// StartHeartbeat begins emitting render's line every interval, but only when output
// is not a live terminal (isLive — so it stays off whenever the animated block is
// on, and runs under NO_COLOR where the block is disabled). render returns the
// current snapshot line (empty to print nothing this tick — e.g. when no work is in
// flight, so an idle watch loop stays silent). The returned Heartbeat is inert on a
// live terminal or when render is nil; Stop is always safe. It runs its own ticker
// because the console's animation ticker only runs while the live block is
// non-empty, which off a live terminal it never is.
func StartHeartbeat(w io.Writer, c Colors, interval time.Duration, render func() string) *Heartbeat {
	h := &Heartbeat{}
	if render == nil || isLive(w, c) {
		return h
	}
	h.stop = make(chan struct{})
	h.done = make(chan struct{})
	go h.run(w, interval, render, h.stop, h.done)
	return h
}

// run is the ticker loop. stop/done are passed in so the goroutine holds its own
// references even after Stop nils the fields.
func (h *Heartbeat) run(w io.Writer, interval time.Duration, render func() string, stop, done chan struct{}) {
	defer close(done)
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			if line := render(); line != "" {
				liveTerm.line(SectionPipeline, w, line+"\n")
			}
		}
	}
}

// Stop ends the heartbeat and waits for its goroutine to exit, so no liveness tick
// can land after Stop returns — the command layer relies on that to keep ticks out
// of the final Summary. Safe on an inert Heartbeat and concurrently/more than once
// (a sync.Once guards the close-and-wait, so a second caller never double-closes).
func (h *Heartbeat) Stop() {
	if h == nil || h.stop == nil {
		return
	}
	h.stopOnce.Do(func() {
		close(h.stop)
		<-h.done
	})
}

// HeartbeatLine formats one liveness line from the in-flight stages: an hourglass,
// then the running stages grouped by kind (🔨 build, 📦 import, 🚢 deploy), each
// named by its owning app (a build/import also by its image label) and how long it
// has been running — "⏳ 🔨 shop/ui 24s · 🚢 db 5s (3 not ready)". A waiting deploy
// also carries its health-gate status, so a stalled one names what it waits on
// rather than only an ever-climbing elapsed. The per-stage elapseds climbing each
// tick are the liveness signal. Returns "" when nothing is running, so the caller
// prints nothing. Stages are sorted (phase, app, label) for stable output.
func HeartbeatLine(c Colors, running []RunningStage) string {
	if len(running) == 0 {
		return ""
	}
	sorted := make([]RunningStage, len(running))
	copy(sorted, running)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Phase != b.Phase {
			return a.Phase < b.Phase
		}
		if a.App != b.App {
			return a.App < b.App
		}
		return a.Label < b.Label
	})

	byPhase := map[int][]string{}
	for _, r := range sorted {
		name := r.App
		if r.Phase != phaseDeploy && r.Label != "" {
			name = r.App + "/" + r.Label
		}
		seg := name + " " + Elapsed(c, r.Elapsed)
		// A waiting deploy carries its health-gate status as a parenthetical so a
		// stalled one names what it waits on. The parens bound the tail's own commas
		// ("3 not ready: a, b") so they never read as item separators in the join.
		if r.Phase == phaseDeploy && r.Tail != "" {
			seg += " " + c.Dim("("+r.Tail+")")
		}
		byPhase[r.Phase] = append(byPhase[r.Phase], seg)
	}
	var groups []string
	for _, ph := range []struct {
		phase int
		icon  string
	}{{phaseBuild, IconBuild}, {phaseImport, IconImport}, {phaseDeploy, IconDeploy}} {
		if items := byPhase[ph.phase]; len(items) > 0 {
			groups = append(groups, ph.icon+" "+strings.Join(items, ", "))
		}
	}
	return fmt.Sprintf("⏳ %s", strings.Join(groups, c.Dim(" · ")))
}
