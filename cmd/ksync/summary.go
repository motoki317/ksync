package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/loop"
	"github.com/motoki317/ksync/internal/ui"
)

// excludedNeedsNote warns when a targeted sync (explicit app names) omits apps
// the selected ones declare in needs: those dependencies are not synced here and
// are assumed already deployed. runByNeeds skips an out-of-set need rather than
// waiting on it, so without this note a missing dependency surfaces only as a
// downstream failure. Empty for an untargeted run (no names) or when nothing is
// excluded.
func excludedNeedsNote(selected []config.App, names []string) string {
	if len(names) == 0 {
		return ""
	}
	in := make(map[string]bool, len(selected))
	for _, a := range selected {
		in[a.Name] = true
	}
	var missing []string
	seen := map[string]bool{}
	for _, a := range selected {
		for _, dep := range a.Needs {
			if !in[dep] && !seen[dep] {
				seen[dep] = true
				missing = append(missing, dep)
			}
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("note: not syncing dependencies %s (not selected); assuming they are already deployed", strings.Join(missing, ", "))
}

// appNames returns the apps' names in order, for scope and confirmation lines.
func appNames(apps []config.App) []string {
	names := make([]string, len(apps))
	for i, a := range apps {
		names[i] = a.Name
	}
	return names
}

// syncStats reduces the engine's per-object results to the apply summary
// (applied/pruned/failed counts) that sync, watch, and destroy all report the
// same way.
func syncStats(results []common.ResourceSyncResult) loop.SyncStats {
	var s loop.SyncStats
	for _, res := range results {
		switch res.Status {
		case common.ResultCodePruned:
			s.Pruned++
		case common.ResultCodeSyncFailed:
			s.Failed++
		default:
			s.Applied++
		}
	}
	return s
}

// summaryBlock renders one app's committed sync block: the ship-emoji apply
// line, preceded by a line per failed resource (✗) and per degraded one (⚠). It
// is what destroy prints directly (a synced app commits the richer pipeline
// block via ui.CommitInfo instead).
func summaryBlock(c ui.Colors, app string, results []common.ResourceSyncResult, degraded []string, took time.Duration, nameW int) string {
	var b strings.Builder
	s := syncStats(results)
	s.Degraded = len(degraded)
	for _, line := range failureLines(c, results, degraded) {
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "%s\n", appSyncLine(c, app, s, took, nameW))
	return b.String()
}

// failureLines lists the per-resource notices that print above an app's committed
// block: a ✗ for each resource the sync failed on, and a ⚠ for each that applied
// cleanly but is broken at runtime (CrashLoop, failed Job) — a sync success the
// developer still needs to see. Empty on a clean sync (the common case).
func failureLines(c ui.Colors, results []common.ResourceSyncResult, degraded []string) []string {
	var lines []string
	for _, res := range results {
		if res.Status == common.ResultCodeSyncFailed {
			lines = append(lines, fmt.Sprintf("  %s %s: %s", c.Red("✗"), res.ResourceKey.String(), res.Message))
		}
	}
	for _, line := range degraded {
		lines = append(lines, fmt.Sprintf("  %s %s", c.Yellow("⚠"), c.Dim(line)))
	}
	return lines
}

// printSummary writes one app's summary block above any live block (destroy has
// no pipeline of its own to commit).
func printSummary(w io.Writer, c ui.Colors, app string, results []common.ResourceSyncResult, degraded []string, took time.Duration, nameW int) {
	ui.WriteLine(ui.SectionPipeline, w, summaryBlock(c, app, results, degraded, took, nameW))
}

// applyParts renders the dim apply summary ("21 applied, 2 pruned, …") and the
// health symbol (✓ applied & healthy · ⚠ applied but a resource is degraded · ✗ a
// sync task failed) shared by the committed deploy row (ui.CommitInfo), the
// deploy-only line, and `ksync destroy`.
func applyParts(c ui.Colors, s loop.SyncStats) (summary, symbol string) {
	parts := []string{fmt.Sprintf("%d applied", s.Applied)}
	if s.Pruned > 0 {
		parts = append(parts, fmt.Sprintf("%d pruned", s.Pruned))
	}
	if s.Failed > 0 {
		parts = append(parts, c.Red(fmt.Sprintf("%d failed", s.Failed)))
	}
	if s.Degraded > 0 {
		parts = append(parts, c.Yellow(fmt.Sprintf("%d degraded", s.Degraded)))
	}
	symbol = c.Green("✓")
	if s.Degraded > 0 {
		symbol = c.Yellow("⚠")
	}
	if s.Failed > 0 {
		symbol = c.Red("✗")
	}
	return c.Dim(strings.Join(parts, ", ")), symbol
}

// appSyncLine renders one app's completed-sync status line via the shared
// ui.DeployLine layout — used by `ksync destroy`, which has no pipeline. It passes
// no kind word: destroy is not a deploy, and its 🚢 icon and summary already say
// what happened. took, when > 0, is appended; a zero duration omits it.
func appSyncLine(c ui.Colors, app string, s loop.SyncStats, took time.Duration, nameW int) string {
	summary, symbol := applyParts(c, s)
	return ui.DeployLine(c, app, "", summary, symbol, took, nameW)
}

// nameColWidth is the app-name column width for the streamed per-app summary
// lines: the run's widest name, so they align — or 0 for a single-app run, which
// needs no padding (and reads cleanest left-tight).
func nameColWidth(apps []config.App) int {
	if len(apps) <= 1 {
		return 0
	}
	w := 0
	for _, a := range apps {
		if len(a.Name) > w {
			w = len(a.Name)
		}
	}
	return w
}

// printPlan opens a whole-stack sync with a titled overview: how many apps, the
// target context, and the app names — so the developer sees the scope before the
// per-app lines start streaming. It emits only the block's own lines; the console
// sets it apart from the surrounding sections (the log above, the pipelines below)
// by the Plan section kind.
func printPlan(w io.Writer, c ui.Colors, apps []config.App, kubeContext string) {
	names := appNames(apps)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", c.Bold("Plan"))
	fmt.Fprintf(&b, "  %s %s %s\n", fmt.Sprintf("%d apps", len(apps)), c.Dim("→"), c.Bold(kubeContext))
	fmt.Fprintf(&b, "  %s\n", c.Dim(strings.Join(names, " ")))
	ui.WriteLine(ui.SectionPlan, w, b.String())
}

// summaryLines renders the titled run-summary block (vitest-style right-aligned
// labels): apps synced, any failed/degraded apps, and the elapsed time. It is
// both the live footer (final=false → "k/N synced", refreshed each tick) and
// the committed close (final=true → "N synced", with degraded apps named).
// Returns the block's lines without trailing newlines.
func summaryLines(c ui.Colors, total, synced int, agg loop.SyncStats, degradedApps []string, elapsed time.Duration, final bool) []string {
	type row struct{ label, value string }
	appsVal := fmt.Sprintf("%d/%d synced", synced, total)
	if final {
		appsVal = fmt.Sprintf("%d synced", synced)
	}
	rows := []row{{"Apps", c.Bold(appsVal)}}
	if agg.Failed > 0 {
		rows = append(rows, row{"Failed", c.Red(fmt.Sprintf("%d failed", agg.Failed))})
	}
	if agg.Degraded > 0 {
		v := c.Yellow(fmt.Sprintf("%d degraded", agg.Degraded))
		// Name the degraded apps only in the committed block — the live footer
		// stays short so a long list cannot wrap and break the pinned block.
		if final && len(degradedApps) > 0 {
			v += c.Dim("  " + strings.Join(degradedApps, ", "))
		}
		rows = append(rows, row{"Degraded", v})
	}
	rows = append(rows, row{"Duration", ui.Duration(elapsed)})

	width := 0
	for _, ln := range rows {
		if len(ln.label) > width {
			width = len(ln.label)
		}
	}
	// No leading blank: the console sets the Summary apart from the section above
	// it — the live footer is spaced from the items by the block's own separator
	// (drawBlock), the committed block by its Summary section kind.
	lines := []string{c.Bold("Summary")}
	for _, ln := range rows {
		// Right-align the label (padding added before color-wrapping, so the
		// columns line up regardless of escape codes), value after a 2-space gap.
		lines = append(lines, fmt.Sprintf("  %s  %s", c.Dim(fmt.Sprintf("%*s", width, ln.label)), ln.value))
	}
	return lines
}

// printSummaryBlock commits the final summary block to scrollback as the Summary
// section, so the console sets it one blank line apart from the pipelines above
// and the watching-for-changes log below.
func printSummaryBlock(w io.Writer, lines []string) {
	ui.WriteLine(ui.SectionSummary, w, strings.Join(lines, "\n")+"\n")
}

// printTimings commits the slowest-first per-app timing recap after the Summary —
// the "which app/image took how long" breakdown a CI reader wants. The entries are
// captured only off a terminal (Recap is a no-op on one), so on a terminal this is
// empty and prints nothing. Its own section means the console sets it one blank
// apart from the Summary above and the watching-for-changes log below.
func printTimings(w io.Writer, c ui.Colors, entries []timingEntry) {
	if len(entries) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString(c.Bold("Timings") + "\n")
	for _, e := range entries {
		b.WriteString("  " + e.line + "\n")
	}
	ui.WriteLine(ui.SectionTimings, w, b.String())
}
