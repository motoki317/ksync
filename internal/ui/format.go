package ui

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"
)

// spinnerFrames is the braille spinner cycle, matching what tools like nix and
// buildkit use for a compact "working" indicator.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// ansiPattern strips the escape sequences and carriage returns that build tools
// emit, so the live tail we show stays on one clean line.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\r`)

// ansiOnly strips escape sequences but KEEPS carriage returns, for a caller that
// interprets "\r" redraws itself (streamClean) — ansiPattern would consume them.
var ansiOnly = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// cols reports the terminal width for fd, or 80 if it cannot be determined.
func cols(fd int) int {
	if w, _, err := term.GetSize(fd); err == nil && w > 0 {
		return w
	}
	return 80
}

// rows reports the terminal height for fd, or 24 if it cannot be determined.
func rows(fd int) int {
	if _, h, err := term.GetSize(fd); err == nil && h > 0 {
		return h
	}
	return 24
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

// streamClean prepares one captured command line for off-terminal streaming: it
// strips ANSI escapes, then collapses a carriage-return redraw to the final text
// the line settled on, and trims trailing space. Unlike sanitizeLine it preserves
// inner spacing, so a build tool's aligned output (buildx's "#5 [2/4] RUN …")
// stays readable rather than collapsed to one space. It strips ANSI with ansiOnly,
// not ansiPattern, precisely so the "\r" survives to be interpreted here.
func streamClean(s string) string {
	s = ansiOnly.ReplaceAllString(s, "")
	// Trim a trailing "\r" (and spaces) first so a plain "\r\n"-terminated line —
	// already stripped of its "\n" upstream — keeps its text instead of vanishing;
	// only then take the text after the last remaining "\r" (the final redraw).
	s = strings.TrimRight(s, "\r \t")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// Elapsed renders a stage's duration as a threshold-colored token — the single
// form shared by the build/import and apply lines so every stage's timing reads
// alike. Color escalates with how long it took (vitest-style): a fast stage is
// green (the happy path), a slower one warms to yellow then red so it draws the
// eye in a stack of otherwise-quiet lines. Thresholds suit a local dev loop,
// where a single-app edit targets a few seconds — past ~10s is worth a glance,
// past a minute is the slow stage to fix. The color alone conveys the tier,
// so no parentheses are needed; the unit letters (m, s) recede into a fainter
// shade of the tier color so the magnitude is what stands out.
func Elapsed(c Colors, d time.Duration) string {
	var bright, faint string
	switch {
	case d >= time.Minute:
		bright, faint = ansiRed, ansiFaintRed
	case d >= 10*time.Second:
		bright, faint = ansiYellow, ansiFaintYellow
	default:
		bright, faint = ansiGreen, ansiFaintGreen
	}
	// The magnitude takes the bright tier code, the unit letters the faint one —
	// so "1m05s" shades its 1 and 05 bright, its m and s faint.
	return formatDuration(d,
		func(num string) string { return c.wrap(bright, num) },
		func(unit string) string { return c.wrap(faint, unit) },
	)
}

// Duration renders an elapsed time compactly for human status lines: "0.4s",
// "12s", "1m03s". Shared by the build/import activity lines and the per-app
// sync status so durations read the same everywhere.
func Duration(d time.Duration) string {
	plain := func(s string) string { return s }
	return formatDuration(d, plain, plain)
}

// formatDuration is the single source of truth for the compact duration layout:
// "0.4s", "12s", "1m03s". It calls num for each magnitude run and unit for each
// unit-letter run, so a caller can style the two differently (Elapsed colors
// them) without re-parsing the formatted string — the number/unit boundaries are
// known here by construction, not rediscovered downstream.
func formatDuration(d time.Duration, num, unit func(string) string) string {
	if d < time.Minute {
		if d < 10*time.Second {
			return num(fmt.Sprintf("%.1f", d.Seconds())) + unit("s")
		}
		return num(fmt.Sprintf("%d", int(d.Seconds()))) + unit("s")
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	return num(fmt.Sprintf("%d", m)) + unit("m") + num(fmt.Sprintf("%02d", s)) + unit("s")
}
