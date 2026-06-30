package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/ui"
)

// offProgress builds a progress whose pipelines are off-terminal (a *bytes.Buffer
// is not an *os.File, so isLive is false) — the mode the heartbeat and the timing
// recap serve.
func offProgress(apps []config.App) (*progress, *bytes.Buffer) {
	var buf bytes.Buffer
	return newProgress(&buf, ui.NewColors(&buf), apps), &buf
}

// snapshot aggregates the in-flight stages across every live pipeline — the seam
// the heartbeat reads each tick. The per-pipeline Snapshot is covered in internal/ui;
// this exercises the cmd-layer map walk: stages from distinct apps all appear, and a
// waiting deploy's health-gate tail survives the hop into the RunningStage set.
func TestProgressSnapshot_AggregatesAcrossPipelines(t *testing.T) {
	prog, _ := offProgress([]config.App{{Name: "shop"}, {Name: "db"}})

	prog.pipeline("shop").Build("ui") // born running
	d := prog.pipeline("db").Deploy()
	d.Start()
	d.SetTail("3 not ready")

	snap := prog.snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot should gather one running stage from each pipeline, got %d: %+v", len(snap), snap)
	}
	var build, deploy *ui.RunningStage
	for i := range snap {
		switch snap[i].App {
		case "shop":
			build = &snap[i]
		case "db":
			deploy = &snap[i]
		}
	}
	if build == nil || build.Label != "ui" {
		t.Errorf("the shop build (label ui) should be in the snapshot, got %+v", snap)
	}
	if deploy == nil || deploy.Tail != "3 not ready" {
		t.Errorf("the db deploy's health-gate tail should survive into the snapshot, got %+v", snap)
	}
}

// Off a terminal, finishing an app captures its completion line for the recap, and
// takeTimings drains and resets that accumulator so the next batch starts empty.
func TestProgressFinish_CapturesAndDrainsTimings(t *testing.T) {
	prog, _ := offProgress([]config.App{{Name: "db"}})

	prog.pipeline("db").Deploy().Start()
	prog.finish("db", &ui.CommitInfo{Summary: "6 applied", Symbol: "✓"})

	entries := prog.takeTimings()
	if len(entries) != 1 || !strings.Contains(entries[0].line, "db") {
		t.Fatalf("finishing an app off a terminal should capture its recap line, got %+v", entries)
	}
	if drained := prog.takeTimings(); len(drained) != 0 {
		t.Errorf("takeTimings should reset the accumulator, second drain got %+v", drained)
	}
}

// takeTimings returns the per-app entries slowest-first, so the recap leads with the
// app that dominated the run's wall-clock.
func TestProgressTakeTimings_SortsSlowestFirst(t *testing.T) {
	prog, _ := offProgress(nil)
	prog.timings = []timingEntry{
		{line: "shop", total: 5 * time.Second},
		{line: "api", total: 30 * time.Second},
		{line: "db", total: 2 * time.Second},
	}
	got := prog.takeTimings()
	order := []string{got[0].line, got[1].line, got[2].line}
	if want := []string{"api", "shop", "db"}; !equalStrings(order, want) {
		t.Errorf("recap should be slowest-first, got %v want %v", order, want)
	}
}

// printTimings titles the recap and lists each entry; an empty recap (a terminal
// run, where Recap is a no-op) prints nothing at all.
func TestPrintTimings_TitlesAndSkipsEmpty(t *testing.T) {
	var buf bytes.Buffer
	c := ui.NewColors(&buf)
	printTimings(&buf, c, nil)
	if buf.String() != "" {
		t.Errorf("an empty recap should print nothing, got %q", buf.String())
	}
	printTimings(&buf, c, []timingEntry{{line: "api  🔨 server 0.2s", total: time.Second}})
	if out := buf.String(); !strings.Contains(out, "Timings") || !strings.Contains(out, "api") {
		t.Errorf("printTimings should title the recap and list the entry, got %q", out)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
