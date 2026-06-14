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

// liveLine serializes the single in-place spinner line: only one Activity may
// own the terminal's current line at a time. Concurrent builds (watch mode,
// multiple apps) that cannot take it fall back to plain start/done lines, so
// their output never clobbers the spinner.
var liveLine sync.Mutex

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
// the full output buffered, printing it only if the command fails.
type Activity struct {
	w     io.Writer
	c     Colors
	label string
	now   func() time.Time
	start time.Time
	live  bool // owns the in-place spinner line
	fd    int

	mu       sync.Mutex
	buf      bytes.Buffer
	lastLine string

	stop    chan struct{}
	stopped chan struct{}
}

// StartActivity begins reporting progress for label (e.g. "build ns-auth-dev").
// When w is an interactive terminal and no other Activity holds the live line,
// it animates a single in-place line; otherwise it prints a plain start line
// and a matching done line, so piped/redirected/concurrent output stays sane.
func StartActivity(w io.Writer, c Colors, label string) *Activity {
	return startActivity(w, c, label, time.Now)
}

func startActivity(w io.Writer, c Colors, label string, now func() time.Time) *Activity {
	a := &Activity{w: w, c: c, label: label, now: now, start: now()}
	f, ok := w.(*os.File)
	isTTY := ok && c.Enabled() && term.IsTerminal(int(f.Fd()))
	if isTTY && liveLine.TryLock() {
		a.live = true
		a.fd = int(f.Fd())
		a.stop = make(chan struct{})
		a.stopped = make(chan struct{})
		go a.animate()
		return a
	}
	_, _ = fmt.Fprintf(w, "%s %s%s\n", c.Cyan("•"), label, c.Dim(" …"))
	return a
}

// Write captures the command's output and remembers its latest non-empty line
// for the live display. It never blocks the command and never errors.
func (a *Activity) Write(p []byte) (int, error) {
	a.mu.Lock()
	a.buf.Write(p)
	if line := lastNonEmptyLine(p); line != "" {
		a.lastLine = sanitizeLine(line)
	}
	a.mu.Unlock()
	return len(p), nil
}

// Done finishes the activity: it stops the spinner, prints a one-line ✓/✗
// summary with the elapsed time, and — only on failure — the full captured
// output so the developer can see what went wrong.
func (a *Activity) Done(err error) {
	if a.live {
		close(a.stop)
		<-a.stopped
		_, _ = fmt.Fprint(a.w, "\r\x1b[K") // erase the spinner line
		liveLine.Unlock()
	}
	elapsed := a.c.Dim("(" + Duration(a.now().Sub(a.start)) + ")")
	if err != nil {
		a.mu.Lock()
		out := strings.TrimRight(a.buf.String(), "\n")
		a.mu.Unlock()
		if out != "" {
			_, _ = fmt.Fprintf(a.w, "%s\n%s\n", a.c.Dim("─── "+a.label+" output ───"), out)
		}
		_, _ = fmt.Fprintf(a.w, "%s %s  %s\n", a.c.Red("✗"), a.label, elapsed)
		return
	}
	_, _ = fmt.Fprintf(a.w, "%s %s  %s\n", a.c.Green("✓"), a.label, elapsed)
}

func (a *Activity) animate() {
	defer close(a.stopped)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-a.stop:
			return
		case <-t.C:
			a.draw(spinnerFrames[i%len(spinnerFrames)])
		}
	}
}

func (a *Activity) draw(frame rune) {
	a.mu.Lock()
	tail := a.lastLine
	a.mu.Unlock()

	elapsed := Duration(a.now().Sub(a.start))
	meta := elapsed
	if tail != "" {
		meta = tail + "  " + elapsed
	}
	// Only the meta tail is truncated; the spinner+label prefix is short and
	// always shown. Width math uses plain rune counts (no color codes yet).
	prefix := string(frame) + " " + a.label + "  "
	if room := a.cols() - len([]rune(prefix)); room > 0 {
		meta = truncateRunes(meta, room)
	}
	_, _ = fmt.Fprintf(a.w, "\r\x1b[K%s %s  %s", a.c.Cyan(string(frame)), a.c.Bold(a.label), a.c.Dim(meta))
}

func (a *Activity) cols() int {
	if w, _, err := term.GetSize(a.fd); err == nil && w > 0 {
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

// truncateRunes shortens s to at most max runes, marking a cut with an ellipsis.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
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
