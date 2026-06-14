package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// A status line printed while a spinner frame is on the line must erase that
// frame first, so the two never share a physical line (the bug this guards:
// "⠋ build x  0.1s✓ app  0 applied").
func TestConsole_LineErasesActiveSpinnerFrame(t *testing.T) {
	var buf bytes.Buffer
	c := &console{}
	c.spinnerFrame(&buf, "⠋ build x  0.1s")
	c.line(&buf, "✓ app  0 applied\n")

	if !strings.Contains(buf.String(), "0.1s"+eraseLine+"✓ app") {
		t.Errorf("status line did not erase the spinner frame first:\n%q", buf.String())
	}
	if c.drawn {
		t.Error("spinner still marked drawn after a line cleared it")
	}
}

// Only the stream the frame was drawn to is erased; a write to a different
// stream must not emit an erase.
func TestConsole_LineOnlyErasesSameStream(t *testing.T) {
	var spin, other bytes.Buffer
	c := &console{}
	c.spinnerFrame(&spin, "⠋ build x")
	c.line(&other, "hello\n")
	if strings.Contains(other.String(), eraseLine) {
		t.Errorf("erased a different stream: %q", other.String())
	}
}

// With no spinner active, a line carries no erase sequence — plain output stays
// plain (matters for piped/redirected runs).
func TestConsole_LineNoEraseWhenClean(t *testing.T) {
	var buf bytes.Buffer
	c := &console{}
	c.line(&buf, "first\n")
	c.line(&buf, "second\n")
	if strings.Contains(buf.String(), eraseLine) {
		t.Errorf("erased with no spinner active: %q", buf.String())
	}
}

// The real bug: a spinner animating in one goroutine while another prints status
// lines. Under -race this also proves the shared state is locked; here we assert
// the invariant — a status line ("LINE") is never glued straight onto a spinner
// frame ("FRAME"), i.e. "FRAMELINE" never appears (there is always an erase
// between). Tokens are chosen so no suffix of one is a prefix of the other.
func TestConsole_ConcurrentSpinnerAndLinesNeverGlue(t *testing.T) {
	var buf bytes.Buffer
	c := &console{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			c.spinnerFrame(&buf, "FRAME")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			c.line(&buf, "LINE\n")
		}
	}()
	wg.Wait()
	if strings.Contains(buf.String(), "FRAMELINE") {
		t.Error("a status line was printed onto a live spinner frame without erasing it")
	}
}
