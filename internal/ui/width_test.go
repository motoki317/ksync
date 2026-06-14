package ui

import (
	"strings"
	"testing"
)

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"abc", 3},
		{"🔨", 2},                 // wide emoji
		{"📦", 2},                 // wide emoji
		{"🔨 rust", 7},            // 2 + 1 + 4
		{"あ", 2},                 // CJK wide
		{"a\x1b[1mb\x1b[0mc", 3}, // ANSI ignored
		{"é", 1},                // e + combining acute is one column
		{"a\u200db", 2},          // zero-width joiner counts nothing
	}
	for _, c := range cases {
		if got := displayWidth(c.s); got != c.want {
			t.Errorf("displayWidth(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

// The invariant the live block depends on: an emoji-prefixed line clamped to a
// narrow budget occupies at most that many display columns — so it cannot wrap
// and throw off the cursor-up erase. This is the exact regression: the 🔨 icon
// (two columns, previously counted as one) pushed lines one past the edge.
func TestClampANSI_KeepsLineWithinBudget(t *testing.T) {
	line := "🔨 rust-services (duo)  loading metadata for a very long image reference"
	for _, budget := range []int{8, 12, 20, 30, 40} {
		got := clampANSI(line, budget)
		if w := displayWidth(got); w > budget {
			t.Errorf("clampANSI(_, %d) width = %d (>%d): %q", budget, w, budget, got)
		}
	}
}

func TestClampANSI_PassesShortLinesThrough(t *testing.T) {
	s := "✓ duo  0 applied"
	if got := clampANSI(s, 80); got != s {
		t.Errorf("clampANSI should not touch a fitting line: %q", got)
	}
}

// Cutting inside colored text must restore the default, or the color bleeds into
// the rest of the terminal.
func TestClampANSI_ResetsColorOnCut(t *testing.T) {
	got := clampANSI("\x1b[1mlong bold heading that overflows the budget\x1b[0m", 8)
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("clamped colored line must end with a reset: %q", got)
	}
	if w := displayWidth(got); w > 8 {
		t.Errorf("clamped colored line width = %d, want <= 8: %q", w, got)
	}
}
