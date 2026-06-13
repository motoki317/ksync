package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestColors_DisabledForNonTerminal(t *testing.T) {
	// A bytes.Buffer is not an *os.File, so color must be off and strings pass
	// through verbatim — the property that keeps piped/redirected output clean.
	c := NewColors(&bytes.Buffer{})
	if c.Enabled() {
		t.Fatal("color enabled for a non-terminal writer")
	}
	if got := c.Red("x"); got != "x" {
		t.Errorf("disabled color altered the string: %q", got)
	}
}

func TestColors_WrapsWhenEnabled(t *testing.T) {
	c := Colors{on: true}
	got := c.Green("ok")
	if !strings.HasPrefix(got, ansiGreen) || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("enabled color did not wrap with SGR codes: %q", got)
	}
}
