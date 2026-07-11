package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/term"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/loop"
	"github.com/motoki317/ksync/internal/ui"
)

// interactiveTerminal reports whether ksync can run the confirmation picker: it
// needs stdin to read raw keystrokes from and stderr (where it draws) to be a
// terminal. Under a pipe, a redirect, or a process manager this is false and the
// loop falls back to auto-rebuild.
func interactiveTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// buildGate drives the interactive confirmation picker for the watch loop. Its
// Ask launches one picker goroutine — the loop guarantees no overlap by issuing
// exactly one prompt at a time — which reads the user's choice via
// ui.ConfirmBuilds and reports it on Decisions; a quit (Ctrl-C) cancels the
// run instead (in raw mode the terminal sends no SIGINT).
type buildGate struct {
	ctx       context.Context
	cancel    context.CancelFunc
	w         io.Writer // stderr: where the picker draws and skip notes print
	out       ui.Colors
	decisions chan loop.Decision

	mu    sync.Mutex
	abort chan struct{} // per-prompt; closed by Abort to refresh the open picker
}

func newBuildGate(ctx context.Context, cancel context.CancelFunc, w io.Writer, out ui.Colors) *loop.Gate {
	g := &buildGate{ctx: ctx, cancel: cancel, w: w, out: out, decisions: make(chan loop.Decision)}
	return &loop.Gate{Ask: g.ask, Abort: g.abortPrompt, Decisions: g.decisions}
}

func (g *buildGate) ask(pending []loop.PendingItem) {
	items := make([]ui.PromptItem, len(pending))
	for i, p := range pending {
		if p.Build == loop.DeployOnly {
			items[i] = ui.PromptItem{Icon: ui.IconDeploy, Label: p.Label, Note: "manifests only"}
		} else {
			items[i] = ui.PromptItem{Icon: ui.IconBuild, Label: p.Label, Note: p.App}
		}
	}
	abort := make(chan struct{})
	g.mu.Lock()
	g.abort = abort
	g.mu.Unlock()
	go func() {
		selected, build, quit, aborted := ui.ConfirmBuilds(g.ctx, abort, os.Stdin, g.w, g.out, items)
		if quit {
			g.cancel()
			return
		}
		if aborted {
			// The loop asked to refresh: report Reask so it re-asks with the now-larger
			// pending set, acting on nothing here.
			select {
			case g.decisions <- loop.Decision{Reask: true}:
			case <-g.ctx.Done():
			}
			return
		}
		var chosen []loop.PendingItem
		if build {
			chosen = make([]loop.PendingItem, 0, len(selected))
			for _, i := range selected {
				chosen = append(chosen, pending[i])
			}
		} else {
			plural := "s"
			if len(pending) == 1 {
				plural = ""
			}
			ui.WriteLine(ui.SectionLog, g.w, g.out.Dim(fmt.Sprintf("— skipped (%d change%s still pending; edit to re-prompt)", len(pending), plural))+"\n")
		}
		select {
		case g.decisions <- loop.Decision{Selected: chosen}:
		case <-g.ctx.Done():
		}
	}()
}

// abortPrompt closes the in-flight prompt's abort channel so ConfirmBuilds returns
// aborted; the gate then reports Reask. Idempotent per prompt (the channel is
// cleared once closed), and a no-op when no prompt is open.
func (g *buildGate) abortPrompt() {
	g.mu.Lock()
	ch := g.abort
	g.abort = nil
	g.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// watchReporter renders the watch loop's per-app sync lines and frames each
// batch — the initial convergence and every later rebuild — the way a `ksync
// sync` run is framed: for a multi-app watch, a Plan up front, then a committed
// Summary block once the batch settles. The initial convergence additionally
// pins a live Summary footer with the running tally while it runs (it can be
// slow); later rebuilds stream their pipelines and commit a Summary when idle.
// Every batch, multi- or single-app, ends with a "watching for changes" log
// line. A single-app watch skips the Plan/footer/Summary and just streams its
// one ship line, like a one-app sync — but still logs the watching line.
//
// The batch tally (synced/agg/degraded) accumulates as apps report and is read
// (under mu) by both the live footer and onIdle, then reset per batch.
type watchReporter struct {
	w     io.Writer
	out   ui.Colors
	log   logr.Logger
	prog  *progress
	total int
	nameW int
	multi bool // frame with a Plan/footer/Summary (more than one app)

	mu       sync.Mutex
	synced   int            // apps synced in the current batch
	agg      loop.SyncStats // apply stats aggregated over the current batch
	degraded []string       // degraded apps in the current batch
	started  time.Time      // initial-convergence start, for the live footer's elapsed
	footer   *ui.Footer     // live tally; pinned only during the initial convergence
}

// startWatchReporter prints the Plan and pins the live Summary footer for a
// multi-app watch (a single-app watch gets neither). Off a terminal there is no
// footer; each app's build streams its log live and commits its result line, so a
// long-building app shows progress without any pinned block.
func startWatchReporter(w io.Writer, out ui.Colors, log logr.Logger, prog *progress, apps []config.App, kubeContext string) *watchReporter {
	r := &watchReporter{w: w, out: out, log: log, prog: prog, total: len(apps), nameW: nameColWidth(apps), multi: len(apps) > 1, started: time.Now()}
	if !r.multi {
		return r
	}
	printPlan(w, out, apps, kubeContext)
	r.footer = ui.StartFooter(w, out, func() []string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return summaryLines(out, r.total, r.synced, r.agg, r.degraded, time.Since(r.started), false)
	})
	return r
}

// report commits one completed app sync (the loop's Report hook): it Finishes
// the app's pipeline — a build app's frozen stage tree or a build-less app's
// ship line — and tallies the sync into the current batch (read back by the
// footer and onIdle). The deploy time on the committed line is the deploy
// stage's own, read from the pipeline, so took is not needed here.
func (r *watchReporter) report(app string, stats loop.SyncStats, _ time.Duration) {
	summary, symbol := applyParts(r.out, stats)
	summary = withRetryCount(summary, r.prog.retriesFor(app))
	info := &ui.CommitInfo{Summary: summary, Symbol: symbol, NameW: r.nameW}
	r.mu.Lock()
	r.synced++
	r.agg.Add(stats)
	if stats.Degraded > 0 {
		r.degraded = append(r.degraded, app)
	}
	r.mu.Unlock()
	r.prog.finish(app, info)
}

// onIdle closes out a batch (the loop's OnIdle hook): it commits the batch's
// Summary block (multi-app only) and logs a watching-for-changes line, then
// resets the per-batch tally for the next one. The initial convergence also has
// a live footer to tear down first, so its pinned block is replaced by the
// committed Summary. took is the batch's wall-clock, from the loop.
func (r *watchReporter) onIdle(took time.Duration) {
	r.mu.Lock()
	synced, agg, degraded := r.synced, r.agg, r.degraded
	r.synced, r.agg, r.degraded = 0, loop.SyncStats{}, nil
	footer := r.footer
	r.footer = nil
	r.mu.Unlock()

	footer.Stop() // nil-safe; present only for the initial convergence
	// Drain the recap every batch to bound memory even on a single-app watch (where
	// it is captured but not printed); print the slowest-first recap after the
	// Summary only for a multi-app batch.
	timings := r.prog.takeTimings()
	if r.multi {
		printSummaryBlock(r.w, summaryLines(r.out, r.total, synced, agg, degraded, took, true))
		printTimings(r.w, r.out, timings)
	}
	// The console sets this log line one blank apart from the Summary (or, for a
	// single-app watch, from the pipeline) above it — see ui.Section.
	r.log.Info("Finished, watching for changes")
}

// stop removes the live footer; idempotent, so the deferred call after the loop
// exits is harmless when a batch already stopped it. It matters when Ctrl-C arrives
// mid-convergence — without it the footer stays pinned.
func (r *watchReporter) stop() {
	r.mu.Lock()
	footer := r.footer
	r.mu.Unlock()
	footer.Stop()
}
