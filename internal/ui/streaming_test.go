package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// resetLiveTerm clears the process-wide console's inter-section bookkeeping so a
// test that writes through it does not leave a stray leading blank for the next
// test (other tests assert exact output through the same singleton).
func resetLiveTerm() {
	liveTerm.mu.Lock()
	liveTerm.lastKind, liveTerm.lastBlank = sectionNone, false
	liveTerm.mu.Unlock()
}

// Off a terminal a build stage streams each complete line of its output live,
// prefixed with the app, label, and phase word, then a result line on Done — the
// buildx model, so a long build's progress shows in a pipe or CI log without any
// synthetic heartbeat.
func TestStage_OffTerminalStreamsBuildLines(t *testing.T) {
	t.Cleanup(resetLiveTerm)
	resetLiveTerm()
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "shop", true, clk)
	b := p.Build("ui")
	_, _ = b.Write([]byte("Compiling foo v0.1.0\nCompiling bar v0.2.0\n"))
	clk.add(61 * time.Second)
	b.Done(nil)

	out := buf.String()
	for _, want := range []string{
		"shop/ui build │ Compiling foo v0.1.0",
		"shop/ui build │ Compiling bar v0.2.0",
		"shop/ui build │ ✓",
		"1m01s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("streamed build output should contain %q, got:\n%s", want, out)
		}
	}
}

// A trailing line with no newline is flushed when the stage finishes, so the last
// line of a build's output is never lost.
func TestStage_OffTerminalFlushesPartialLineOnDone(t *testing.T) {
	t.Cleanup(resetLiveTerm)
	resetLiveTerm()
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "shop", true, clk)
	b := p.Build("ui")
	_, _ = b.Write([]byte("final line without newline"))
	b.Done(nil)
	if !strings.Contains(buf.String(), "shop/ui build │ final line without newline") {
		t.Errorf("a trailing partial line should flush on Done, got:\n%s", buf.String())
	}
}

// streamClean strips ANSI and resolves carriage-return redraws to the line's
// final visible text, preserving inner spacing. The CRLF case is the one that
// regressed before (ansiPattern ate the "\r" so the redraw-slice was dead code).
func TestStreamClean(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain keeps inner spacing", "#5 [2/4] RUN echo  hi", "#5 [2/4] RUN echo  hi"},
		{"CRLF residue keeps text", "building\r", "building"},
		{"interior CR takes final", "a\rb", "b"},
		{"multiple CR takes last", "10%\r50%\r100%", "100%"},
		{"ANSI erase before redraw", "\x1b[2K\rDownloading 50%", "Downloading 50%"},
		{"ANSI color stripped", "\x1b[32mok\x1b[0m", "ok"},
		{"trailing spaces trimmed", "done   ", "done"},
		{"empty", "", ""},
		// A line cleared with spaces after a "\r" keeps its last visible frame
		// rather than blanking — benign and out of scope (true clear semantics need
		// terminal emulation); plain build output (--progress=plain) never emits it.
		{"space-cleared keeps last frame", "progress\r   ", "progress"},
	} {
		if got := streamClean(tc.in); got != tc.want {
			t.Errorf("%s: streamClean(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// A carriage-return redraw within one line streams only the final text the line
// settled on, not the overwritten prefix — so a "\r"-updated progress line reads
// cleanly off a terminal instead of concatenating every redraw frame.
func TestStage_OffTerminalCollapsesCarriageReturnRedraw(t *testing.T) {
	t.Cleanup(resetLiveTerm)
	resetLiveTerm()
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "shop", true, clk)
	b := p.Build("ui")
	_, _ = b.Write([]byte("Downloading 10%\rDownloading 100%\n"))
	b.Done(nil)

	out := buf.String()
	if !strings.Contains(out, "shop/ui build │ Downloading 100%") {
		t.Errorf("a carriage-return redraw should stream its final text, got:\n%s", out)
	}
	if strings.Contains(out, "Downloading 10%Downloading 100%") {
		t.Errorf("the overwritten prefix must not survive the redraw, got:\n%s", out)
	}
}

// A build failure off a terminal shows the streamed lines and a ✗ result, but no
// re-dumped "output" block — the lines already streamed, so re-dumping would
// double them.
func TestStage_OffTerminalBuildFailureNoReDump(t *testing.T) {
	t.Cleanup(resetLiveTerm)
	resetLiveTerm()
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "shop", true, clk)
	b := p.Build("ui")
	_, _ = b.Write([]byte("step 1\nERROR: it broke\n"))
	b.Done(errors.New("exit 1"))

	out := buf.String()
	if !strings.Contains(out, "shop/ui build │ ERROR: it broke") {
		t.Errorf("the failing line should have streamed, got:\n%s", out)
	}
	if !strings.Contains(out, "shop/ui build │ ✗") {
		t.Errorf("a build failure should print a ✗ result line, got:\n%s", out)
	}
	if strings.Contains(out, "output ───") {
		t.Errorf("a build's output already streamed, so it must not be re-dumped, got:\n%s", out)
	}
	if n := strings.Count(out, "ERROR: it broke"); n != 1 {
		t.Errorf("the failing line should appear exactly once, got %d:\n%s", n, out)
	}
}

// An import stage off a terminal does not stream its lines (build.Loader coalesces
// a load and tees one command to every waiting app, so per-stage streaming would
// duplicate each line). On success it prints only a result line; on failure it
// dumps the captured output so the break is diagnosable.
func TestStage_OffTerminalImportNoStreamDumpsOnFailure(t *testing.T) {
	t.Cleanup(resetLiveTerm)
	resetLiveTerm()
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "shop", true, clk)
	i := p.Import("ui")
	_, _ = i.Write([]byte("importing image abc\n"))
	i.Done(nil)
	if strings.Contains(buf.String(), "importing image abc") {
		t.Errorf("import output should not stream line-by-line off a terminal, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "shop/ui import │ ✓") {
		t.Errorf("a successful import should print a result line, got:\n%s", buf.String())
	}

	resetLiveTerm()
	buf.Reset()
	clk = &clock{t: time.Unix(0, 0)}
	p = newPipe(&buf, "shop", true, clk)
	i = p.Import("ui")
	_, _ = i.Write([]byte("importing image abc\nload error: no space\n"))
	i.Done(errors.New("exit 2"))
	out := buf.String()
	if !strings.Contains(out, "load error: no space") {
		t.Errorf("an import failure should dump its captured output, got:\n%s", out)
	}
	if !strings.Contains(out, "shop/ui import │ ✗") {
		t.Errorf("an import failure should print a ✗ result line, got:\n%s", out)
	}
}

// Off a terminal the deploy stage streams a health-gate line only when the status
// changes — never a timed reprint of the same not-ready set — and prints no result
// line of its own on Done (the committed deploy line is its result).
func TestStage_OffTerminalDeployStreamsOnChangeOnly(t *testing.T) {
	t.Cleanup(resetLiveTerm)
	resetLiveTerm()
	clk := &clock{t: time.Unix(0, 0)}
	var buf bytes.Buffer
	p := newPipe(&buf, "db", false, clk)
	d := p.Deploy()
	d.Start()
	d.SetTail("waiting for health 2 not ready: a, b")
	d.SetTail("waiting for health 2 not ready: a, b") // unchanged: no second line
	d.SetTail("waiting for health 1 not ready: a")    // changed: streams
	clk.add(11 * time.Second)
	d.Done(nil)

	out := buf.String()
	if n := strings.Count(out, "db deploy │ waiting for health 2 not ready"); n != 1 {
		t.Errorf("an unchanged deploy status must not reprint (want 1, got %d):\n%s", n, out)
	}
	if !strings.Contains(out, "db deploy │ waiting for health 1 not ready: a") {
		t.Errorf("a changed deploy status should stream, got:\n%s", out)
	}
	if strings.Contains(out, "deploy │ ✓") || strings.Contains(out, "deploy │ ✗") {
		t.Errorf("the deploy stage prints no result line of its own off a terminal, got:\n%s", out)
	}
}
