package ui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// spinnerFrames is the braille spinner cycle, matching what tools like nix and
// buildkit use for a compact "working" indicator.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// ansiPattern strips the escape sequences and carriage returns that build tools
// emit, so the live tail we show stays on one clean line.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\r`)

// Activity is the nix-style progress line for one long external command (a
// docker build, an image import). It is the io.Writer the command's combined
// output is wired to: instead of streaming every line, it shows a single
// updating line — spinner, label, the latest output line, elapsed — and keeps
// the full output buffered, printing it only if the command fails. Concurrent
// activities each get their own line; liveTerm renders them as one block (see
// console.go), so several builds animate at once instead of one blocking the
// rest.
type Activity struct {
	w     io.Writer
	c     Colors
	label string
	now   func() time.Time
	start time.Time
	track *track // non-nil when animating on a terminal; nil on the plain path

	mu  sync.Mutex
	buf bytes.Buffer
}

// StartActivity begins reporting progress for label (e.g. "build ns-auth-dev").
// On an interactive terminal it adds an animated line to liveTerm's live block;
// otherwise it prints a plain start line (and a matching done line), so
// piped/redirected output stays sane.
func StartActivity(w io.Writer, c Colors, label string) *Activity {
	return startActivity(w, c, label, time.Now)
}

func startActivity(w io.Writer, c Colors, label string, now func() time.Time) *Activity {
	a := &Activity{w: w, c: c, label: label, now: now, start: now()}
	f, ok := w.(*os.File)
	isTTY := ok && c.Enabled() && term.IsTerminal(int(f.Fd()))
	if isTTY {
		fd := int(f.Fd())
		a.track = &track{
			label:  label,
			colors: c,
			start:  a.start,
			now:    now,
		}
		liveTerm.addTrack(w, func() int { return cols(fd) }, a.track)
		return a
	}
	// Non-interactive: a plain start line, through liveTerm so it interleaves
	// correctly with any other plain output.
	liveTerm.line(w, fmt.Sprintf("%s %s%s\n", c.Cyan("•"), label, c.Dim(" …")))
	return a
}

// Write captures the command's output and feeds its latest non-empty line to
// the live line. It never blocks the command and never errors.
func (a *Activity) Write(p []byte) (int, error) {
	a.mu.Lock()
	a.buf.Write(p)
	a.mu.Unlock()
	if a.track != nil {
		if line := lastNonEmptyLine(p); line != "" {
			a.track.setTail(sanitizeLine(line))
		}
	}
	return len(p), nil
}

// Done finishes the activity: it removes the live line, prints a one-line ✓/✗
// summary with the elapsed time, and — only on failure — the full captured
// output so the developer can see what went wrong.
func (a *Activity) Done(err error) {
	elapsed := a.c.Dim("(" + Duration(a.now().Sub(a.start)) + ")")
	var b strings.Builder
	if err != nil {
		a.mu.Lock()
		out := strings.TrimRight(a.buf.String(), "\n")
		a.mu.Unlock()
		if out != "" {
			fmt.Fprintf(&b, "%s\n%s\n", a.c.Dim("─── "+a.label+" output ───"), out)
		}
		fmt.Fprintf(&b, "%s %s  %s\n", a.c.Red("✗"), a.label, elapsed)
	} else {
		fmt.Fprintf(&b, "%s %s  %s\n", a.c.Green("✓"), a.label, elapsed)
	}
	if a.track != nil {
		liveTerm.finishTrack(a.track, b.String())
		return
	}
	liveTerm.line(a.w, b.String())
}

// cols reports the terminal width for fd, or 80 if it cannot be determined.
func cols(fd int) int {
	if w, _, err := term.GetSize(fd); err == nil && w > 0 {
		return w
	}
	return 80
}

// lastNonEmptyLine returns the last non-blank line within p, split on both \n
// and \r so carriage-return progress updates surface their latest text.
func lastNonEmptyLine(p []byte) string {
	fields := bytes.FieldsFunc(p, func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(fields) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(string(fields[i])); s != "" {
			return s
		}
	}
	return ""
}

// sanitizeLine removes escape sequences and collapses inner whitespace so a
// build tool's line renders cleanly inside the single live line.
func sanitizeLine(s string) string {
	s = ansiPattern.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}

// Duration renders an elapsed time compactly for human status lines: "0.4s",
// "12s", "1m03s". Shared by the build/import activity lines and the per-app
// sync status so durations read the same everywhere.
func Duration(d time.Duration) string {
	if d < time.Minute {
		if d < 10*time.Second {
			return fmt.Sprintf("%.1fs", d.Seconds())
		}
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	return fmt.Sprintf("%dm%02ds", m, s)
}
