package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSanitizeLine(t *testing.T) {
	cases := map[string]string{
		"\x1b[36m#11 1.79 building\x1b[0m": "#11 1.79 building",
		"resolved   1,   reused 0\r":       "resolved 1, reused 0",
		"  plain  ":                        "plain",
	}
	for in, want := range cases {
		if got := sanitizeLine(in); got != want {
			t.Errorf("sanitizeLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		s    string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"日本語テスト", 3, "日本…"}, // rune-aware, not byte-aware
	}
	for _, c := range cases {
		if got := truncateRunes(c.s, c.max); got != c.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
		}
	}
}

func TestDuration(t *testing.T) {
	cases := map[time.Duration]string{
		400 * time.Millisecond: "0.4s",
		12 * time.Second:       "12s",
		63 * time.Second:       "1m03s",
	}
	for d, want := range cases {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	cases := map[string]string{
		"first\nsecond\n":          "second",
		"a\r\nb\r\n":               "b",
		"trailing\n\n   \n":        "trailing",
		"progress: 1\rprogress: 2": "progress: 2",
		"":                         "",
	}
	for in, want := range cases {
		if got := lastNonEmptyLine([]byte(in)); got != want {
			t.Errorf("lastNonEmptyLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// A bytes.Buffer is not a terminal, so the activity takes the plain path:
// a start line, captured output, and a ✓/✗ done line with no spinner codes.
func TestActivity_PlainPathSuccess(t *testing.T) {
	var buf bytes.Buffer
	clk := func() time.Time { return time.Unix(0, 0) }
	a := startActivity(&buf, Colors{}, "build api-b", clk)
	_, _ = a.Write([]byte("step 1\nstep 2\n"))
	a.Done(nil)

	got := buf.String()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("plain path emitted escape codes: %q", got)
	}
	if !strings.Contains(got, "build api-b") {
		t.Errorf("missing label: %q", got)
	}
	if !strings.Contains(got, "✓") {
		t.Errorf("missing success marker: %q", got)
	}
	// On success the captured output stays hidden.
	if strings.Contains(got, "step 2") {
		t.Errorf("success path leaked build output: %q", got)
	}
}

func TestActivity_PlainPathFailureDumpsOutput(t *testing.T) {
	var buf bytes.Buffer
	clk := func() time.Time { return time.Unix(0, 0) }
	a := startActivity(&buf, Colors{}, "build api-b", clk)
	_, _ = a.Write([]byte("compiling\nerror: boom\n"))
	a.Done(errors.New("exit 1"))

	got := buf.String()
	if !strings.Contains(got, "✗") {
		t.Errorf("missing failure marker: %q", got)
	}
	// On failure the full output is shown for debugging.
	if !strings.Contains(got, "error: boom") {
		t.Errorf("failure path did not dump build output: %q", got)
	}
}
