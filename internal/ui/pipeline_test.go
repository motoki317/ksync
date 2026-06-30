package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// clock is a hand-advanced time source so stage elapsed times are deterministic.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) add(d time.Duration) time.Time {
	c.t = c.t.Add(d)
	return c.t
}

// newPipe builds a pipeline on a non-terminal writer (tty=false, so the live
// console singleton is never touched) with color off (plain, assertable text),
// for testing render output via lines() and the plain Done/Finish paths.
func newPipe(buf *bytes.Buffer, app string, expand bool, clk *clock) *Pipeline {
	return startPipeline(buf, Colors{}, app, expand, clk.now)
}

// A build app's pipeline renders an app header with its stages as rows, named by
// kind (Build/Import/Deploy), ordered build→import→deploy regardless of when
// each was added, and with the not-yet-started deploy shown as a pending row.
func TestPipeline_TreeNamesAndOrdersStages(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	p.Deploy() // added first, but phase-ordered last
	p.Build("ns-dashboard")
	p.Build("go-components (5)")

	lines := p.lines('⠹')
	if len(lines) != 4 {
		t.Fatalf("want header + 3 rows, got %d lines: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "ns-system") {
		t.Errorf("line 0 should be the app header, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "Build") || !strings.Contains(lines[1], "ns-dashboard") {
		t.Errorf("row 1 should be the first build, got %q", lines[1])
	}
	if !strings.Contains(lines[2], "Build") || !strings.Contains(lines[2], "go-components (5)") {
		t.Errorf("row 2 should be the group build, got %q", lines[2])
	}
	// Deploy sorts last even though it was added first, and shows pending (○)
	// while the builds run — the "future stage" the developer can see waiting.
	if !strings.Contains(lines[3], "Deploy") || !strings.Contains(lines[3], "○") {
		t.Errorf("row 3 should be the pending deploy, got %q", lines[3])
	}
	if !strings.Contains(lines[3], IconDeploy) {
		t.Errorf("deploy row should lead with the deploy icon, got %q", lines[3])
	}
}

// The deploy row transitions pending → running → done, and a finished stage
// reports its elapsed time.
func TestPipeline_StageLifecycle(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	d := p.Deploy()
	p.Build("img")

	if row := p.lines('⠹')[2]; !strings.Contains(row, "○") {
		t.Errorf("deploy should start pending, got %q", row)
	}
	d.Start()
	d.SetTail("waiting for health  3 not ready")
	if row := p.lines('⠹')[2]; !strings.Contains(row, "⠹") || !strings.Contains(row, "3 not ready") {
		t.Errorf("running deploy should show the spinner and its tail, got %q", row)
	}
	clk.add(2 * time.Second)
	d.Done(nil)
	if row := p.lines('⠹')[2]; !strings.Contains(row, "✓") || !strings.Contains(row, "2.0s") {
		t.Errorf("done deploy should show ✓ and elapsed, got %q", row)
	}
}

// An app with no builds (expand=false, deploy only) collapses to one line with
// the app name inline — infra apps stay compact instead of growing a header.
func TestPipeline_DeployOnlyCollapses(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "db", false, clk)
	p.Deploy()

	lines := p.lines('⠼')
	if len(lines) != 1 {
		t.Fatalf("deploy-only app should collapse to one line, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "Deploy") || !strings.Contains(lines[0], "db") {
		t.Errorf("collapsed line should name the deploy and the app, got %q", lines[0])
	}
}

// A build app never collapses, even before any build row is added: the deploy
// pending row shows under the header so the upcoming work is visible at once.
func TestPipeline_ExpandNeverCollapses(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	p.Deploy()
	if lines := p.lines('⠼'); len(lines) != 2 {
		t.Fatalf("expanded pipeline should keep the header even with one stage, got %q", lines)
	}
}

// Off a terminal a build app streams its per-stage result lines as they finish
// (the buildx model — line-by-line output is covered in streaming_test.go), then
// Finish commits just the deploy result line; the slowest-first recap (Recap)
// carries the per-app total.
func TestPipeline_NonTTYBuildStreamsThenCommitsDeployLine(t *testing.T) {
	t.Cleanup(resetLiveTerm)
	resetLiveTerm()
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	b := p.Build("img")
	clk.add(time.Second)
	b.Done(nil)
	// The build's result line streamed as it finished, so the buffer is not empty.
	if !strings.Contains(buf.String(), "ns-system/img build │ ✓") {
		t.Errorf("a build's result line should stream as it finishes, got %q", buf.String())
	}
	d := p.Deploy()
	d.Start()
	clk.add(500 * time.Millisecond)
	d.Done(nil)

	buf.Reset()
	p.Finish(CommitInfo{Summary: "3 applied", Symbol: "✓"})
	out := strings.TrimSpace(buf.String())
	if strings.Contains(out, "\n") {
		t.Errorf("a build app should commit just its deploy line off a terminal, got %q", out)
	}
	for _, want := range []string{"ns-system", IconDeploy, "Deploy", "3 applied", "0.5s"} {
		if !strings.Contains(out, want) {
			t.Errorf("committed deploy line should contain %q, got %q", want, out)
		}
	}
}

// A finished build app commits its full stage tree — each build/import row and
// the deploy row keep their final time — instead of collapsing to the deploy
// line, so the per-stage timings survive after the run.
func TestPipeline_CommittedTreeKeepsStageTimes(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	d := p.Deploy()
	b := p.Build("img")
	clk.add(80 * time.Second)
	b.Done(nil)
	i := p.Import("img")
	clk.add(8 * time.Second)
	i.Done(nil)
	d.Start()
	clk.add(112 * time.Second)
	d.Done(nil)

	lines := p.committedTree(CommitInfo{Summary: "21 applied", Symbol: "✓"})
	// The header is the app name followed by the group's total wall-clock: builds
	// overlap, so the span runs from the first build start to the deploy end
	// (0→80→88→200s here = 3m20s), not the sum of the rows.
	if !strings.HasPrefix(lines[0], "ns-system") || !strings.Contains(lines[0], "3m20s") {
		t.Errorf("header should be the app name with the group total 3m20s, got %q", lines[0])
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, IconBuild) || !strings.Contains(joined, "img") || !strings.Contains(joined, "1m20s") {
		t.Errorf("committed tree should keep the build row with its time, got:\n%s", joined)
	}
	if !strings.Contains(joined, IconImport) || !strings.Contains(joined, "8.0s") {
		t.Errorf("committed tree should keep the import row with its time, got:\n%s", joined)
	}
	if !strings.Contains(joined, "deploy") || !strings.Contains(joined, "21 applied") || !strings.Contains(joined, "1m52s") {
		t.Errorf("committed deploy row should carry the apply summary and its time, got:\n%s", joined)
	}
}

// A deploy-only app commits one line that leads with the "Deploy" kind word — so
// it reads consistently with the deploy row of a build app's tree — names the app,
// and carries its apply summary. The time shown is the deploy stage's own, not an
// end-to-end wall clock.
func TestPipeline_CommittedLineCarriesDeployWord(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "db", false, clk)
	d := p.Deploy()
	d.Start()
	clk.add(32 * time.Second)
	d.Done(nil)

	line := p.committedLine(CommitInfo{Summary: "40 applied", Symbol: "✓"})
	if !strings.Contains(line, IconDeploy) || !strings.Contains(line, "Deploy") || !strings.Contains(line, "db") {
		t.Errorf("committed line should lead with the 🚢 icon, the Deploy word, and the app, got %q", line)
	}
	if !strings.Contains(line, "40 applied") || !strings.Contains(line, "32s") {
		t.Errorf("committed line should carry the apply summary and the deploy stage's own time, got %q", line)
	}
}

// The live tree header carries the group's running total — the wall-clock span
// across its stages — so the whole app's elapsed is visible at a glance, not only
// the per-stage rows.
func TestPipeline_LiveHeaderShowsGroupTotal(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "sistema", true, clk)
	p.Deploy()
	p.Build("ui") // running from t=0
	clk.add(12 * time.Second)

	header := p.lines('⠹')[0]
	if !strings.Contains(header, "sistema") || !strings.Contains(header, "12s") {
		t.Errorf("live header should show the app and its running group total, got %q", header)
	}
}

// A build app whose images were all supplied as overrides never builds, so only
// the deploy stage exists. While the deploy is pending it keeps its header row
// (the upcoming work shows), but once the deploy starts it collapses to a single
// line and commits as one deploy line — never a one-child tree.
func TestPipeline_OverrideOnlyDeployCollapses(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "duo", true, clk) // expand: has builds in config, all overridden this run
	d := p.Deploy()

	if got := p.lines('⠼'); len(got) != 2 {
		t.Fatalf("a pending deploy-only build app keeps its header row, got %q", got)
	}
	d.Start()
	clk.add(37 * time.Second)
	d.Done(nil)
	if got := p.lines('⠼'); len(got) != 1 {
		t.Fatalf("a started deploy-only build app collapses to one line, got %q", got)
	}
	if p.hasBuildStages() {
		t.Errorf("an app that only deployed should report no build stages")
	}
	line := p.committedLine(CommitInfo{Summary: "6 applied", Symbol: "✓"})
	if !strings.Contains(line, "Deploy") || !strings.Contains(line, "duo") || !strings.Contains(line, "6 applied") {
		t.Errorf("override-only app should commit as a single Deploy line, got %q", line)
	}
}

// A failed stage off a terminal is diagnosable: a build's lines already streamed
// (each app/label/phase-prefixed), and Done adds a ✗ result line.
func TestPipeline_FailureShowsCapturedOutput(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	b := p.Build("img")
	_, _ = b.Write([]byte("step 1/3\nERROR: it broke\n"))
	b.Done(errors.New("exit 1"))

	out := buf.String()
	if !strings.Contains(out, "ERROR: it broke") {
		t.Errorf("failure should stream the captured output, got %q", out)
	}
	// Each streamed line is app/label/phase-prefixed, so a failure names which app
	// built what.
	if !strings.Contains(out, "ns-system/img build │") {
		t.Errorf("streamed lines should be app/label/phase-prefixed, got %q", out)
	}
}

// Off a terminal, Recap returns the app's completion line and the group's total
// wall-clock (the span across overlapping stages) for the slowest-first recap.
func TestPipeline_RecapOffTerminal(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "shop", true, clk)
	d := p.Deploy()
	b := p.Build("ui")
	clk.add(30 * time.Second)
	b.Done(nil)
	d.Start()
	clk.add(10 * time.Second)
	d.Done(nil)

	line, total, ok := p.Recap(CommitInfo{Summary: "5 applied", Symbol: "✓"})
	if !ok {
		t.Fatal("Recap should report off a terminal")
	}
	if total != 40*time.Second {
		t.Errorf("total should be the group span (build start → deploy end = 40s), got %s", total)
	}
	for _, want := range []string{"shop", "ui", "5 applied"} {
		if !strings.Contains(line, want) {
			t.Errorf("recap line should contain %q, got %q", want, line)
		}
	}
}

// On a terminal Recap is a no-op: the frozen stage trees already show per-app
// timings in scrollback, so there is no separate recap to print.
func TestPipeline_RecapSilentOnTerminal(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "shop", true, clk)
	p.tty = true // pretend a terminal
	if _, _, ok := p.Recap(CommitInfo{}); ok {
		t.Error("Recap should be a no-op on a terminal")
	}
}

// On a terminal Stage is the io.Writer a command's output is wired to: the latest
// non-empty line surfaces as the running row's tail, carriage-return updates
// included. (Off a terminal the line streams instead — see streaming_test.go.)
func TestStage_WriteTailsLatestLine(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	p.tty = true // exercise the on-terminal tail path
	b := p.Build("img")
	_, _ = b.Write([]byte("pulling base\rexporting layers"))

	if row := p.lines('⠹')[1]; !strings.Contains(row, "exporting layers") {
		t.Errorf("running build row should tail the latest output line, got %q", row)
	}
}
