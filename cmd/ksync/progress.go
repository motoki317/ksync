package main

import (
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/ui"
)

// progress coordinates the per-app pipelines (ui.Pipeline) that group an app's
// build, import, and deploy stages under one header. One pipeline per app run:
// the build hook attaches the build/import rows, the deploy attaches its row,
// and the orchestrator commits the group with the app's summary line. Keyed by
// app name — an app never runs concurrently with itself (one-shot sync runs each
// app's fn once; the watch scheduler serializes per app) — so the key names
// exactly one live pipeline.
type progress struct {
	w      io.Writer
	colors ui.Colors
	expand map[string]bool // app -> has builds: show the full tree, never collapse

	mu      sync.Mutex
	pipes   map[string]*ui.Pipeline
	timings []timingEntry  // off-terminal: finished apps' recap lines, drained per batch
	retries map[string]int // app -> times its last sync retried, for the committed "(N retries)" suffix
}

// timingEntry is one app's contribution to the slowest-first timing recap: its
// completion line (identical to what Finish printed) and the group's total
// wall-clock, the sort key.
type timingEntry struct {
	line  string
	total time.Duration
}

func newProgress(w io.Writer, colors ui.Colors, apps []config.App) *progress {
	expand := make(map[string]bool, len(apps))
	for _, a := range apps {
		expand[a.Name] = len(a.Build) > 0
	}
	return &progress{w: w, colors: colors, expand: expand, pipes: make(map[string]*ui.Pipeline)}
}

// takeTimings drains the accumulated per-app recap entries, sorted slowest-first,
// and resets the accumulator for the next batch. Called every batch (one-shot run,
// or each watch convergence) to bound memory even when the recap is not printed.
func (p *progress) takeTimings() []timingEntry {
	p.mu.Lock()
	entries := p.timings
	p.timings = nil
	p.mu.Unlock()
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].total > entries[j].total })
	return entries
}

// recordRetries stores how many times app's current sync retried, read back by
// retriesFor when the deploy line is committed. Set every retry event so the
// final value is the sync's total.
func (p *progress) recordRetries(app string, n int) {
	p.mu.Lock()
	if p.retries == nil {
		p.retries = make(map[string]int)
	}
	p.retries[app] = n
	p.mu.Unlock()
}

// retriesFor returns and clears app's recorded retry count, so the next run
// starts from zero.
func (p *progress) retriesFor(app string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.retries[app]
	delete(p.retries, app)
	return n
}

// pipeline returns the app's live pipeline, creating it (with a pending Deploy
// row, so the deploy shows as upcoming work while builds run) on first use this
// run. The build hook and the deploy share the one returned per app.
func (p *progress) pipeline(app string) *ui.Pipeline {
	p.mu.Lock()
	defer p.mu.Unlock()
	pipe := p.pipes[app]
	if pipe == nil {
		pipe = ui.StartPipeline(p.w, p.colors, app, p.expand[app])
		pipe.Deploy() // pending; rendered beneath the builds
		p.pipes[app] = pipe
	}
	return pipe
}

// finish commits the app's pipeline (the frozen committed block printed in place
// of the live group) and clears it so the next run starts fresh. A nil info
// removes the group silently — a failed run whose error surfaces elsewhere.
func (p *progress) finish(app string, info *ui.CommitInfo) {
	p.mu.Lock()
	pipe := p.pipes[app]
	delete(p.pipes, app)
	p.mu.Unlock()
	if pipe != nil {
		if info == nil {
			pipe.Discard()
			return
		}
		pipe.Finish(*info)
		// Off a terminal, retain this app's completion line and total for the
		// slowest-first recap printed after the run's Summary — the live pipes map is
		// empty by then, so the recap cannot be read from it (Recap is a no-op on a
		// terminal, where the frozen trees already show timings in scrollback).
		if line, total, ok := pipe.Recap(*info); ok {
			p.mu.Lock()
			p.timings = append(p.timings, timingEntry{line: line, total: total})
			p.mu.Unlock()
		}
		return
	}
	// No live group for this app (a report with no prior deploy, as in tests, or a
	// path that opened none): still surface the committed line — without the live
	// pipeline there is no deploy stage to time, so the line omits its duration.
	if info != nil {
		var b strings.Builder
		for _, ln := range info.Above {
			b.WriteString(ln + "\n")
		}
		b.WriteString(ui.DeployLine(p.colors, app, "Deploy", info.Summary, info.Symbol, 0, info.NameW) + "\n")
		ui.WriteLine(ui.SectionPipeline, p.w, b.String())
	}
}
