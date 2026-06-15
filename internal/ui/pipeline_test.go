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

// Off a terminal, build/import stages print plain result lines (their only
// record) but the deploy is silent — its committed summary already reports the
// app, so a pipe shows one line per app, not two.
func TestPipeline_NonTTYBuildPrintsDeploySilent(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	b := p.Build("img")
	clk.add(time.Second)
	b.Done(nil)
	d := p.Deploy()
	d.Start()
	clk.add(500 * time.Millisecond)
	d.Done(nil)
	p.Finish("✓ 🚢 ns-system  3 applied  1.5s\n")

	out := buf.String()
	if !strings.Contains(out, "Build img") {
		t.Errorf("non-tty build should print a plain result line, got %q", out)
	}
	if strings.Contains(out, "Deploy ns-system") {
		t.Errorf("non-tty deploy must be silent (the summary is its record), got %q", out)
	}
	if !strings.Contains(out, "3 applied") {
		t.Errorf("Finish should print the committed summary, got %q", out)
	}
}

// A failed stage surfaces its full captured output (not just the ✗) so a build
// failure is diagnosable even on a pipe.
func TestPipeline_FailureShowsCapturedOutput(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	b := p.Build("img")
	_, _ = b.Write([]byte("step 1/3\nERROR: it broke\n"))
	b.Done(errors.New("exit 1"))

	out := buf.String()
	if !strings.Contains(out, "ERROR: it broke") {
		t.Errorf("failure should print the captured output, got %q", out)
	}
	if !strings.Contains(out, "Build img") {
		t.Errorf("failure log should be headed by the stage, got %q", out)
	}
}

// Stage is the io.Writer a command's output is wired to: the latest non-empty
// line surfaces as the running row's tail, carriage-return updates included.
func TestStage_WriteTailsLatestLine(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "ns-system", true, clk)
	b := p.Build("img")
	_, _ = b.Write([]byte("pulling base\rexporting layers"))

	if row := p.lines('⠹')[1]; !strings.Contains(row, "exporting layers") {
		t.Errorf("running build row should tail the latest output line, got %q", row)
	}
}
