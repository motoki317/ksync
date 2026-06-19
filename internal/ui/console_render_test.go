package ui

import (
	"bytes"
	"strings"
	"testing"
)

// vterm is a minimal terminal model: it replays the exact control bytes the
// console emits — CR, newline (ONLCR: cursor to column 0 of the next row), clear-
// to-end-of-line (\x1b[K), and cursor-up (\x1b[1A) — to reconstruct the final
// on-screen text. The live block is painted then erased/redrawn in place, so a
// plain byte buffer cannot show what the developer ends up seeing; replaying the
// control codes does. It exists to pin the one thing callers kept getting wrong by
// hand: exactly one blank line between sections, and none missing or doubled.
type vterm struct {
	lines [][]rune
	row   int
	col   int
}

func (v *vterm) at(row int) []rune {
	for row >= len(v.lines) {
		v.lines = append(v.lines, nil)
	}
	return v.lines[row]
}

func (v *vterm) put(r rune) {
	line := v.at(v.row)
	for v.col > len(line) {
		line = append(line, ' ')
	}
	if v.col < len(line) {
		line[v.col] = r
	} else {
		line = append(line, r)
	}
	v.lines[v.row] = line
	v.col++
}

func (v *vterm) feed(s string) {
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch {
		case rs[i] == '\n':
			v.row++
			v.col = 0
			v.at(v.row)
		case rs[i] == '\r':
			v.col = 0
		case rs[i] == 0x1b && i+1 < len(rs) && rs[i+1] == '[':
			// CSI: consume until the final letter; handle the two the console uses.
			j := i + 2
			for j < len(rs) && (rs[j] >= '0' && rs[j] <= '9') {
				j++
			}
			if j < len(rs) {
				switch rs[j] {
				case 'K': // clear from the cursor to end of line
					line := v.at(v.row)
					if v.col < len(line) {
						v.lines[v.row] = line[:v.col]
					}
				case 'A': // cursor up one row (column unchanged)
					if v.row > 0 {
						v.row--
					}
				}
				i = j
			}
		default:
			v.put(rs[i])
		}
	}
}

// screen returns the reconstructed lines, right-trimmed, with a single trailing
// empty line dropped (the cursor sits at column 0 of a fresh row after the last
// committed newline) — so the result is exactly the committed scrollback.
func (v *vterm) screen() []string {
	out := make([]string, len(v.lines))
	for i, ln := range v.lines {
		out[i] = strings.TrimRight(string(ln), " ")
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// renderConsole replays everything the console wrote to buf into a vterm and
// returns the final on-screen lines.
func renderConsole(buf *bytes.Buffer) []string {
	v := &vterm{}
	v.feed(buf.String())
	return v.screen()
}

// blanksAround reports the indices in lines that are blank, so a test can assert
// where the separators landed without pinning the exact (colored) content.
func nonBlankCount(lines []string) int {
	n := 0
	for _, ln := range lines {
		if ln != "" {
			n++
		}
	}
	return n
}

// A whole multi-app convergence and a follow-up rebuild must read as cleanly
// separated sections: exactly one blank line between the log line and the Plan,
// the Plan and the streamed pipelines, the pipelines and the Summary, and the
// Summary and the watching line — and the rebuild that follows must be set off
// from the watching line above it (the bug this guards: it glued straight on).
func TestConsole_SectionSpacing(t *testing.T) {
	var buf bytes.Buffer
	c := &console{w: &buf, cols: cols80, rows: bigRows}

	// Convergence: a watching log line, the Plan, the live Summary footer, two app
	// pipelines that finish, then the committed Summary and the watching line.
	c.line(SectionLog, &buf, "Watching for changes\n")
	c.line(SectionPlan, &buf, "Plan\n  2 apps → ctx\n  team-a api-b\n")
	c.setFooter(&buf, cols80, bigRows, func() []string { return []string{"Summary", "  Apps  0/2 synced"} })
	a := item("⠋ team-a  building")
	c.addItem(&buf, cols80, bigRows, a)
	c.finishItem(a, "✓ team-a  0 applied  1.0s\n")
	b := item("⠋ api-b  building")
	c.addItem(&buf, cols80, bigRows, b)
	c.finishItem(b, "✓ api-b  0 applied  1.0s\n")
	c.clearFooter()
	c.line(SectionSummary, &buf, "Summary\n  Apps  2 synced\n  Duration  2.0s\n")
	c.line(SectionLog, &buf, "Finished, watching for changes\n")

	want := []string{
		"Watching for changes",
		"",
		"Plan",
		"  2 apps → ctx",
		"  team-a api-b",
		"",
		"✓ team-a  0 applied  1.0s",
		"✓ api-b  0 applied  1.0s",
		"",
		"Summary",
		"  Apps  2 synced",
		"  Duration  2.0s",
		"",
		"Finished, watching for changes",
	}
	if got := renderConsole(&buf); !equalLines(got, want) {
		t.Fatalf("convergence scrollback mismatch:\n got %#v\nwant %#v", got, want)
	}

	// The reported bug: a rebuild after the watching line glued onto it. The block
	// starts fresh, so it must be one blank line below the watching line.
	r := item("⠋ team-a  building")
	c.addItem(&buf, cols80, bigRows, r)
	c.finishItem(r, "team-a  1.3s\n  ✓ 🔨 build  1.0s\n  ✓ 🚢 deploy  0 applied  0.1s\n")
	c.line(SectionLog, &buf, "Finished, watching for changes\n")

	got := renderConsole(&buf)
	// Find the first watching line; the next non-empty line must be the rebuild
	// header, with exactly one blank between them.
	idx := -1
	for i, ln := range got {
		if ln == "Finished, watching for changes" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+2 >= len(got) {
		t.Fatalf("rebuild scrollback too short:\n%#v", got)
	}
	if got[idx+1] != "" || got[idx+2] != "team-a  1.3s" {
		t.Errorf("rebuild must sit one blank line below the watching line, got:\n%#v", got[idx:])
	}
}

// No two adjacent blank lines and no leading blank, across the same sequence —
// the structural guarantee, independent of the exact content above.
func TestConsole_NoDoubleOrLeadingBlanks(t *testing.T) {
	var buf bytes.Buffer
	c := &console{w: &buf, cols: cols80, rows: bigRows}

	c.line(SectionLog, &buf, "Watching for changes\n")
	c.line(SectionPlan, &buf, "Plan\n  1 apps → ctx\n  team-a\n")
	c.setFooter(&buf, cols80, bigRows, func() []string { return []string{"Summary", "  Apps  0/1 synced"} })
	a := item("⠋ team-a  building")
	c.addItem(&buf, cols80, bigRows, a)
	c.finishItem(a, "✓ team-a  0 applied  1.0s\n")
	c.clearFooter()
	c.line(SectionSummary, &buf, "Summary\n  Apps  1 synced\n  Duration  1.0s\n")
	c.line(SectionLog, &buf, "Finished, watching for changes\n")

	got := renderConsole(&buf)
	if len(got) == 0 || got[0] == "" {
		t.Errorf("output must not start with a blank line, got:\n%#v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] == "" && got[i-1] == "" {
			t.Errorf("two adjacent blank lines at %d, got:\n%#v", i, got)
		}
	}
	if nonBlankCount(got) == 0 {
		t.Errorf("expected committed content, got nothing")
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
