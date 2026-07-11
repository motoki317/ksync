package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/ui"
)

// diagnoseTimeout bounds how long the post-timeout diagnostic gather may take,
// so a wedged cluster cannot turn a sync timeout into a hang on the dump.
const diagnoseTimeout = 15 * time.Second

// reportSyncDiagnostics gathers and prints a debug block for an app whose sync
// ended before it became healthy — the unhealthy resources' events plus related
// pods' container state and logs — so the reader sees what wedged it at a glance.
//
// It runs for a health-gate timeout always, and for a cancellation only when
// userInterrupted — a real Ctrl-C of a one-shot `ksync sync`. A user stops a wedged
// deploy by hitting Ctrl-C, not by waiting out a multi-minute --timeout, and still
// wants to see why it was stuck. A sibling app's failure also cancels this app's
// context, but that is not a user interrupt: the failed sibling reports its own
// error, and dumping every still-progressing app on top of it would be noise, so
// userInterrupted gates those out. The watch loop never sets it — there Ctrl-C is a
// routine quit, not a debugging moment.
//
// A second Ctrl-C force-quits (ADR 20260627-double-signal-force-quit) before this
// bounded gather completes, so the wait is always escapable. Diagnose self-selects
// the unhealthy resources, so an app merely mid-rollout (nothing unhealthy when
// interrupted) prints nothing.
//
// The gather runs on a fresh context.Background() bounded by diagnoseTimeout, never
// the sync's own context (which the timeout or cancellation has just closed —
// reading against it returns only "context canceled"). The block is written to
// scrollback as pipeline output rather than through the single-line logger, which
// cannot carry a multi-line dump.
func reportSyncDiagnostics(eng *engine.Engine, w io.Writer, out ui.Colors, app, namespace string, objs []*unstructured.Unstructured, err error, userInterrupted bool) {
	if !shouldDiagnose(err, userInterrupted) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), diagnoseTimeout)
	defer cancel()
	lines := eng.Diagnose(ctx, app, namespace, objs)
	if len(lines) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString(out.Bold("Diagnostics") + " " + out.Dim("— "+app+" did not become healthy") + "\n")
	for _, ln := range lines {
		b.WriteString(ln + "\n")
	}
	ui.WriteLine(ui.SectionPipeline, w, b.String())
}

// shouldDiagnose reports whether a failed sync warrants a diagnostic dump: a
// health-gate timeout always, and a cancellation only when the user interrupted
// the run (a sibling-abort cancellation is excluded — see reportSyncDiagnostics).
func shouldDiagnose(err error, userInterrupted bool) bool {
	var te *engine.TimeoutError
	if errors.As(err, &te) {
		return true
	}
	return userInterrupted && errors.Is(err, context.Canceled)
}
