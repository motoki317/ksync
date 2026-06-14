package ui

import (
	"strings"
	"unicode"

	"golang.org/x/text/width"
)

// The live block paints each logical line on exactly one physical terminal row
// and erases by moving the cursor up one row per line. That accounting only
// holds if no line is wider than the terminal — a single wrapped line throws off
// every subsequent erase and the block corrupts (the classic symptom: spinner
// frames piling up instead of updating in place). Measuring width by rune count
// breaks this the moment a wide rune appears: an emoji like 🔨 is one rune but
// two columns, so a line sized by rune count overflows by one and wraps. These
// helpers measure and clamp by true display width so the one-row invariant holds.

// runeWidth returns the number of terminal columns r occupies: 0 for combining
// marks, variation selectors and zero-width format characters; 2 for East-Asian
// Wide/Fullwidth runes (CJK and most emoji); 1 otherwise.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r): // combining marks, variation selectors
		return 0
	case r == 0x200d, r == 0xfeff, r >= 0x200b && r <= 0x200f: // ZWJ and zero-width format chars
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// ansiAt reports the CSI escape sequence beginning at rs[i] (e.g. a "\x1b[1m"
// color code) and its length in runes, or ("", 0) if none starts there. Such
// sequences are emitted but occupy no columns, so width math must skip them.
func ansiAt(rs []rune, i int) (string, int) {
	if rs[i] != 0x1b || i+1 >= len(rs) || rs[i+1] != '[' {
		return "", 0
	}
	j := i + 2
	for j < len(rs) && (rs[j] < 0x40 || rs[j] > 0x7e) { // CSI ends at its final byte 0x40–0x7E
		j++
	}
	if j < len(rs) {
		j++ // include the final byte
	}
	return string(rs[i:j]), j - i
}

// displayWidth returns how many terminal columns s occupies, ignoring ANSI
// escape sequences and counting each rune by runeWidth.
func displayWidth(s string) int {
	rs := []rune(s)
	w := 0
	for i := 0; i < len(rs); {
		if _, n := ansiAt(rs, i); n > 0 {
			i += n
			continue
		}
		w += runeWidth(rs[i])
		i++
	}
	return w
}

// clampANSI truncates s so its visible content occupies at most max display
// columns, marking a cut with an ellipsis. ANSI escape sequences are copied
// through verbatim and never counted; if the cut falls inside colored text a
// reset is appended so color cannot bleed past the line. A line that already
// fits is returned unchanged. This is the single guarantee the live block relies
// on: every line it paints is at most the terminal's width, hence one row.
func clampANSI(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if displayWidth(s) <= max {
		return s
	}
	budget := max - 1 // reserve one column for the ellipsis
	rs := []rune(s)
	var b strings.Builder
	w, sawSGR := 0, false
	for i := 0; i < len(rs); {
		if seq, n := ansiAt(rs, i); n > 0 {
			b.WriteString(seq)
			sawSGR = sawSGR || strings.HasSuffix(seq, "m")
			i += n
			continue
		}
		rw := runeWidth(rs[i])
		if w+rw > budget {
			break
		}
		b.WriteRune(rs[i])
		w += rw
		i++
	}
	b.WriteString("…")
	if sawSGR {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}
