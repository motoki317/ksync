package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/engine"
	"github.com/motoki317/ksync/internal/render"
	"github.com/motoki317/ksync/internal/ui"
)

// appDiff is one app's preview: the per-resource diffs, the build repos whose
// live dev tag was carried forward (suppressed from the diff), and the build
// repos shown at their source ref because no live dev tag was found (so the note
// can warn that the displayed tag is not what a sync would deploy).
type appDiff struct {
	app      config.App
	diffs    []engine.ResourceDiff
	carried  []string
	atSrcRef []string
}

// runDiff renders each selected app exactly as `sync` would (post-render patches,
// image overrides, and live build-tag carry-forward) and prints a per-resource
// unified YAML diff against live cluster state. Read-only: no apply, no build.
func runDiff(args []string) error {
	fs := newSubFlagSet("diff")
	prune := fs.Bool("prune", true, "show tracked resources a sync would prune (delete)")
	maxParallel := maxParallelFlag(fs)
	offline := offlineRenderFlag(fs)
	clientDiff := fs.Bool("client-diff", false, "diff in-process (client-side) instead of via a server-side dry-run apply; faster, but fields the cluster defaults or prunes can show as drift")
	var images stringSlice
	fs.Var(&images, "image", "diff as if this pre-built image were deployed: `IMAGE=REF` (repeatable; also via "+overrideEnv+")")
	kctx := contextFlag(fs)
	cfg, names, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	apps, err := cfg.Select(names)
	if err != nil {
		return err
	}
	apps = config.SortByNeeds(apps) // deterministic order for the printed sections
	kubeContext, err := resolveContext(cfg, *kctx)
	if err != nil {
		return err
	}

	appLog, engineLog := setupLogging(false)
	overrides, err := imageOverrides(cfg, images, appLog)
	if err != nil {
		return err
	}
	eng, err := engine.New(kubeContext, engineLog)
	if err != nil {
		return err
	}
	defer eng.Close()

	renderOpts, cleanup, err := renderOptions(kubeContext, *offline)
	if err != nil {
		return err
	}
	defer cleanup()
	r := render.New(renderOpts)
	lookup := render.NewVarLookup(cfg.Dir())

	results, err := renderConcurrently(apps, *maxParallel, func(app config.App) (appDiff, error) {
		return diffApp(r, eng, lookup, app, overrides, *prune, !*clientDiff)
	})
	if err != nil {
		return err
	}

	out := ui.NewColors(os.Stdout)
	if _, err := fmt.Fprint(os.Stdout, formatDiff(out, results)); err != nil {
		return err
	}
	return nil
}

// diffApp renders one app and previews its sync: patches and any --image override
// are applied as on sync; for an un-overridden build image the current live dev
// tag is carried forward so its per-build churn does not dominate the diff.
func diffApp(r *render.Renderer, eng *engine.Engine, lookup func(string) (string, bool), app config.App, overrides map[string]render.Image, prune, serverSide bool) (appDiff, error) {
	res, err := r.Render(app.Path, app.ClientRender)
	if err != nil {
		return appDiff{}, fmt.Errorf("app %s: %w", app.Name, err)
	}
	if err := res.ApplyPatches(app.Patches, lookup); err != nil {
		return appDiff{}, fmt.Errorf("app %s: %w", app.Name, err)
	}
	// An overridden build image deploys the supplied ref (shown in the diff); the
	// rest are un-built, so carry their live dev tag forward. A build entry whose
	// image the manifests do not actually reference is skipped — carrying or noting
	// it would be phantom churn (deployed is read before the override rewrite, but
	// overridden entries are handled separately so that does not matter).
	deployed := res.ReferencedRepos()
	var supplied []render.Image
	var buildRepos []string
	for j := range app.Build {
		img := app.Build[j].Image
		if ov, ok := overrides[img]; ok {
			supplied = append(supplied, ov)
		} else if deployed[img] {
			buildRepos = append(buildRepos, img)
		}
	}
	if len(supplied) > 0 {
		if err := res.SetImages(supplied); err != nil {
			return appDiff{}, fmt.Errorf("app %s: %w", app.Name, err)
		}
	}
	var carried []string
	if len(buildRepos) > 0 {
		live, err := eng.LiveObjects(app.Name, app.Namespace, res.Objects)
		if err != nil {
			return appDiff{}, fmt.Errorf("app %s: %w", app.Name, err)
		}
		if carried, err = res.CarryForwardBuildTags(live, buildRepos); err != nil {
			return appDiff{}, fmt.Errorf("app %s: %w", app.Name, err)
		}
	}
	diffs, err := eng.Diff(app.Name, res.Objects, engine.DiffOptions{Prune: prune, Namespace: app.Namespace, ServerSide: serverSide})
	if err != nil {
		return appDiff{}, fmt.Errorf("app %s: %w", app.Name, err)
	}
	return appDiff{app: app, diffs: diffs, carried: carried, atSrcRef: subtract(buildRepos, carried)}, nil
}

// subtract returns the elements of all not present in remove, preserving order —
// the build repos whose live dev tag carry-forward did not find, so their image
// shows at its source ref rather than the dev tag a sync would build.
func subtract(all, remove []string) []string {
	if len(remove) == 0 {
		return all
	}
	gone := make(map[string]struct{}, len(remove))
	for _, r := range remove {
		gone[r] = struct{}{}
	}
	var out []string
	for _, a := range all {
		if _, ok := gone[a]; !ok {
			out = append(out, a)
		}
	}
	return out
}

// formatDiff renders the whole-run diff: a section per app that has changes
// (each resource's header and colorized unified-YAML hunks, plus a note for any
// carried-forward build tag), the apps already in sync named once, and a closing
// count. Pure, so the layout is unit-tested with colors off.
//
// An app with a fail-open build image (atSrcRef) is never "in sync" even with no
// resource hunks: a sync rebuilds it to a fresh ksync tag the diff cannot predict,
// so its image will change. Its section prints to carry the note, and the global
// "No changes" line is suppressed when any section was emitted.
func formatDiff(c ui.Colors, results []appDiff) string {
	var b strings.Builder
	var inSync []string
	var creates, updates, prunes, sections int
	for _, ad := range results {
		if len(ad.diffs) == 0 && len(ad.atSrcRef) == 0 {
			inSync = append(inSync, ad.app.Name)
			continue
		}
		sections++
		fmt.Fprintf(&b, "%s\n", c.Bold(ad.app.Name))
		for _, d := range ad.diffs {
			switch d.Type {
			case engine.DiffCreate:
				creates++
			case engine.DiffPrune:
				prunes++
			default:
				updates++
			}
			b.WriteString(resourceDiffBlock(c, d))
		}
		if len(ad.carried) > 0 {
			fmt.Fprintf(&b, "  %s\n", c.Dim("note: carried forward live dev tag for "+strings.Join(ad.carried, ", ")+" (not rebuilt)"))
		}
		if len(ad.atSrcRef) > 0 {
			fmt.Fprintf(&b, "  %s\n", c.Dim("note: "+strings.Join(ad.atSrcRef, ", ")+" shown at source ref; sync rebuilds to a fresh dev tag"))
		}
		b.WriteByte('\n')
	}
	if sections == 0 {
		return c.Dim("No changes; the cluster matches the rendered apps.") + "\n"
	}
	if len(inSync) > 0 {
		fmt.Fprintf(&b, "%s\n\n", c.Dim("In sync: "+strings.Join(inSync, ", ")))
	}
	b.WriteString(diffSummaryLine(c, creates, updates, prunes))
	return b.String()
}

// resourceDiffBlock renders one resource's diff: a header naming the change
// (symbol, Kind/name, namespace, verb) and the indented, colorized unified-YAML
// hunks beneath it.
func resourceDiffBlock(c ui.Colors, d engine.ResourceDiff) string {
	var sym, verb func(string) string
	var mark string
	switch d.Type {
	case engine.DiffCreate:
		mark, sym, verb = "+", c.Green, c.Green
	case engine.DiffPrune:
		mark, sym, verb = "-", c.Red, c.Red
	default:
		mark, sym, verb = "~", c.Yellow, c.Yellow
	}
	name := d.Kind + "/" + d.Name
	header := fmt.Sprintf("  %s %s", sym(mark), c.Bold(name))
	if d.Namespace != "" {
		header += "  " + c.Dim(d.Namespace)
	}
	header += "  " + verb(d.Type.String())
	body := colorizeDiff(c, unifiedDiff(d.Before, d.After))
	return header + "\n" + body
}

// unifiedDiff is the unified diff of two YAML strings, with no `---`/`+++` file
// header (the resource header above replaces it). An empty side yields an
// all-added or all-removed block (a creation or prune).
func unifiedDiff(before, after string) string {
	s, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:       splitDiffLines(before),
		B:       splitDiffLines(after),
		Context: 3,
	})
	if err != nil {
		// GetUnifiedDiffString only fails on a writer error (a bytes.Buffer never
		// errors); keep the resource visible rather than aborting the run.
		return fmt.Sprintf("    (diff render failed: %v)\n", err)
	}
	return s
}

// splitDiffLines splits a YAML side into lines for difflib, returning nil for an
// empty side so a creation/prune does not show a phantom blank line.
func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	return difflib.SplitLines(s)
}

// colorizeDiff indents each unified-diff line by four spaces and colors it by its
// marker: green for additions, red for removals, cyan for hunk ranges, dim for
// context.
func colorizeDiff(c ui.Colors, diff string) string {
	if diff == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		var colored string
		switch {
		case strings.HasPrefix(line, "+"):
			colored = c.Green(line)
		case strings.HasPrefix(line, "-"):
			colored = c.Red(line)
		case strings.HasPrefix(line, "@@"):
			colored = c.Cyan(line)
		default:
			colored = c.Dim(line)
		}
		b.WriteString("    " + colored + "\n")
	}
	return b.String()
}

// diffSummaryLine is the closing count of what a sync would change across the run.
func diffSummaryLine(c ui.Colors, creates, updates, prunes int) string {
	parts := []string{
		fmt.Sprintf("%d to create", creates),
		fmt.Sprintf("%d to update", updates),
		fmt.Sprintf("%d to prune", prunes),
	}
	return c.Bold("Summary") + "  " + c.Dim(strings.Join(parts, ", ")) + "\n"
}
