// Package ui is ksync's human-facing output layer. Logs are the only interface
// ksync has with the developer running it, so this package owns two concerns:
// turning the engine's structured log stream into clean, colored, scannable
// lines (see Sink), and the small color helpers that the command layer reuses
// for its own summaries. Color is opt-out (NO_COLOR) and auto-disabled when the
// target is not a terminal, so piping or redirecting yields plain text.
package ui

import (
	"io"
	"os"

	"golang.org/x/term"
)

// ANSI SGR sequences. Kept here so every colored string in ksync agrees on the
// same palette.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[90m" // bright black: readable on both light and dark themes
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"

	// Faint (SGR 2) combined with a hue: a dimmer shade of the same color, used
	// to recede a duration's unit letters behind its digits. Terminals that don't
	// support faint degrade to the plain hue, which is acceptable.
	ansiFaintRed    = "\x1b[2;31m"
	ansiFaintGreen  = "\x1b[2;32m"
	ansiFaintYellow = "\x1b[2;33m"
)

// Colors renders ANSI-colored strings, or plain ones when color is disabled, so
// call sites never branch on whether color is on.
type Colors struct{ on bool }

// NewColors decides whether to colorize output for w: never when NO_COLOR is
// set (the de-facto standard) or when w is not a terminal (pipes, files, CI).
func NewColors(w io.Writer) Colors {
	return Colors{on: colorEnabled(w)}
}

func colorEnabled(w io.Writer) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// Enabled reports whether these Colors emit escape codes.
func (c Colors) Enabled() bool { return c.on }

func (c Colors) wrap(code, s string) string {
	if !c.on {
		return s
	}
	return code + s + ansiReset
}

func (c Colors) Bold(s string) string   { return c.wrap(ansiBold, s) }
func (c Colors) Dim(s string) string    { return c.wrap(ansiDim, s) }
func (c Colors) Red(s string) string    { return c.wrap(ansiRed, s) }
func (c Colors) Green(s string) string  { return c.wrap(ansiGreen, s) }
func (c Colors) Yellow(s string) string { return c.wrap(ansiYellow, s) }
func (c Colors) Cyan(s string) string   { return c.wrap(ansiCyan, s) }
