package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// liveTerm coordinates all of ksync's human-facing terminal output. Apps sync
// concurrently, so several per-app progress groups (pipelines) may be in flight
// at once; liveTerm renders them together as a block of live lines pinned to the
// bottom of the terminal. Any ordinary status line (a committed app summary) is
// printed *above* that block: liveTerm erases the block, writes the line, then
// repaints the block below it. One ticker drives the animation for every item.
// One process-wide instance is enough — all of ksync's human output goes to one
// terminal (stderr).
//
// This is what keeps concurrent work legible: without it, a summary written
// straight to stderr mid-spinner produced runs like
// "⠋ ns-system  0.1s✓ alloy  0 applied".
//
// (Named liveTerm, not term, because golang.org/x/term already owns that name.)
var liveTerm = &console{}

// eraseLine returns the cursor to column 0 and clears to end of line.
const eraseLine = "\r\x1b[K"

// cursorUp moves the cursor up one row (column is unchanged).
const cursorUp = "\x1b[1A"

// liveMargin is how many columns the block holds back from the terminal's full
// width. East-Asian width is not perfectly predictable in every terminal — an
// emoji with a variation selector (e.g. ☸️) may render as two columns where the
// Unicode tables say one — and many terminals defer the wrap when a glyph lands
// in the final column (auto-margin). Painting strictly narrower than the screen
// absorbs both so a line can never wrap.
const liveMargin = 2

// Section labels the kind of output a committed write belongs to, so the console
// can keep distinct sections (a Plan, the streamed pipelines, the run Summary, a
// log line) set apart by exactly one blank line without any caller hand-rolling
// the spacing. The console inserts a separating blank whenever consecutive writes
// carry different kinds — the one place that owns inter-section spacing, so it is
// consistent and a new kind of output is separated automatically. The zero value
// (sectionNone) means nothing has been written yet, so the top of output has no
// leading blank.
type Section int

const (
	sectionNone Section = iota
	SectionLog
	SectionPlan
	SectionSummary
	SectionTimings
	SectionPipeline
)

// blockItem is one unit of the live block: it renders to one or more complete
// lines for the given spinner frame (not width-limited — the console clamps each
// line when painting). A pipeline (a per-app progress group) is the only
// implementation; modeling the block as a list of items keeps the erase/redraw
// machinery independent of what is being shown.
type blockItem interface {
	lines(frame rune) []string
}

type console struct {
	mu     sync.Mutex
	w      io.Writer       // the terminal the live block renders to; set on first use
	cols   func() int      // terminal width source, bound alongside w
	rows   func() int      // terminal height source, bound alongside w
	items  []blockItem     // active progress groups, top-to-bottom in start order
	footer func() []string // optional pinned block (the live run summary), rendered below the items
	shown  int             // how many block lines are currently on screen
	frame  int             // spinner frame index, advanced by the ticker

	lastKind  Section // section of the last committed write, for inter-section spacing
	lastBlank bool    // the last committed write ended in a blank line (no separator due)

	ticker *time.Ticker
	stop   chan struct{}
}

// needSep reports whether a separating blank line is due before a section of the
// given kind: only when something of a different kind was already written and the
// previous write did not itself end in a blank line — so separators never double
// up and the very top of output carries none. Caller holds mu.
func (c *console) needSep(kind Section) bool {
	return c.lastKind != sectionNone && c.lastKind != kind && !c.lastBlank
}

// endsWithBlankLine reports whether s (a complete write, trailing newline included)
// leaves the cursor on a blank line — i.e. its last rendered line is empty.
func endsWithBlankLine(s string) bool {
	return s == "\n" || strings.HasSuffix(s, "\n\n")
}

// attach binds the output stream and its size sources the first time the block
// is used; later items/footers on the same terminal reuse them. Caller holds mu.
func (c *console) attach(w io.Writer, cols, rows func() int) {
	if c.w == nil {
		c.w, c.cols, c.rows = w, cols, rows
	}
}

// budget is the per-line column limit: the terminal width less liveMargin (or a
// sane default off a terminal). Every painted line is clamped to it.
func (c *console) budget() int {
	w := 80
	if c.cols != nil {
		if n := c.cols(); n > 0 {
			w = n
		}
	}
	if w-liveMargin < 1 {
		return 1
	}
	return w - liveMargin
}

// rowBudget is the most lines the block may occupy: one short of the terminal
// height, so a committed line printed above it never scrolls the top row out of
// view mid-erase. Without a height source (tests, non-terminals) there is no cap
// — a very tall default — preserving the simple "paint everything" behavior.
func (c *console) rowBudget() int {
	if c.rows == nil {
		return 1 << 30
	}
	if n := c.rows(); n > 1 {
		return n - 1
	}
	return 1
}

// blockEmpty reports whether nothing is being rendered (no items, no footer);
// the ticker runs exactly while the block is non-empty. Caller holds mu.
func (c *console) blockEmpty() bool { return len(c.items) == 0 && c.footer == nil }

// addItem registers a live progress group and (re)paints the block. The first
// item to arrive fixes the output stream and size sources and starts the
// animation ticker.
func (c *console) addItem(w io.Writer, cols, rows func() int, it blockItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attach(w, cols, rows)
	if c.blockEmpty() {
		c.beginBlock()
	}
	c.eraseBlock()
	c.items = append(c.items, it)
	c.drawBlock()
	c.ensureTicker()
}

// beginBlock separates a freshly starting live block (the first pipeline group or
// the run-summary footer) from the previous section, treating the whole block as
// one Pipeline section. Caller holds mu, the block is currently empty, and c.w is
// bound (attach ran). It emits only the permanent leading blank; drawBlock paints
// the transient lines just after. Marking the section here means the block's own
// commits (item done-lines) do not separate from one another.
func (c *console) beginBlock() {
	if c.needSep(SectionPipeline) {
		_, _ = io.WriteString(c.w, "\n")
	}
	c.lastKind, c.lastBlank = SectionPipeline, true
}

// finishItem removes a live group, printing its committed summary above whatever
// items remain. The ticker stops once the last item and the footer are gone.
func (c *console) finishItem(it blockItem, doneLine string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eraseBlock()
	if doneLine != "" {
		if c.needSep(SectionPipeline) {
			_, _ = io.WriteString(c.w, "\n")
		}
		_, _ = io.WriteString(c.w, doneLine)
		c.lastKind, c.lastBlank = SectionPipeline, endsWithBlankLine(doneLine)
	}
	for i, x := range c.items {
		if x == it {
			c.items = append(c.items[:i], c.items[i+1:]...)
			break
		}
	}
	c.drawBlock()
	if c.blockEmpty() {
		c.stopTicker()
	}
}

// refresh repaints the block in place — used when a group's structure changes
// (a stage added, a stage state transition) between animation ticks so the
// change shows immediately. A no-op when nothing is on screen.
func (c *console) refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shown == 0 && c.blockEmpty() {
		return
	}
	c.eraseBlock()
	c.drawBlock()
}

// setFooter pins render's lines below the items (the live run summary),
// re-rendered on every tick. It keeps the block (and the ticker) alive on its
// own, so the summary stays visible — and updating — after the last group
// finishes. render is called from the ticker goroutine, so it must be safe to
// call concurrently with the caller's own state updates.
func (c *console) setFooter(w io.Writer, cols, rows func() int, render func() []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attach(w, cols, rows)
	if c.blockEmpty() {
		c.beginBlock()
	}
	c.eraseBlock()
	c.footer = render
	c.drawBlock()
	c.ensureTicker()
}

// clearFooter removes the pinned summary; the caller prints the final summary
// itself (so it also lands in piped/CI output, where no footer ever ran). The
// ticker stops once the block is empty.
func (c *console) clearFooter() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eraseBlock()
	c.footer = nil
	c.drawBlock()
	if c.blockEmpty() {
		c.stopTicker()
	}
}

// line writes one complete status line (s includes its trailing newline) of the
// given section to permanent scrollback, above any live block.
func (c *console) line(kind Section, w io.Writer, s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commit(kind, w, s)
}

// commit writes s above the live block — erase the block, separate from the prior
// section if the kind changed, write the line, repaint the block — recording the
// section so the next write knows whether a blank is due. With no block active it
// is the same logic over a no-op erase/repaint, so piped/redirected output stays
// clean (just the optional separating blank between differing sections). Caller
// holds mu. w is the target only before any block has bound c.w (pure-log runs).
func (c *console) commit(kind Section, w io.Writer, s string) {
	target := c.w
	if target == nil {
		target = w
	}
	c.eraseBlock()
	if c.needSep(kind) {
		_, _ = io.WriteString(target, "\n")
	}
	_, _ = io.WriteString(target, s)
	c.lastKind, c.lastBlank = kind, endsWithBlankLine(s)
	c.drawBlock()
}

// eraseBlock clears the on-screen block, leaving the cursor at column 0 of where
// the block's top line was. Caller holds mu.
func (c *console) eraseBlock() {
	if c.shown == 0 {
		return
	}
	_, _ = io.WriteString(c.w, eraseLine) // clear the (cursor's) last line
	for i := 1; i < c.shown; i++ {
		_, _ = io.WriteString(c.w, cursorUp+eraseLine)
	}
	c.shown = 0
}

// drawBlock paints the active items top-to-bottom followed by the footer,
// leaving the cursor at the end of the last line (no trailing newline, so the
// block stays in place). Caller holds mu and has just erased any prior block.
func (c *console) drawBlock() {
	frame := spinnerFrames[c.frame%len(spinnerFrames)]
	var itemLines []string
	for _, it := range c.items {
		itemLines = append(itemLines, it.lines(frame)...)
	}
	var footerLines []string
	if c.footer != nil {
		// A blank row sets the pinned footer (the run summary) apart from the live
		// item lines above it; with no items the block-start blank already does.
		if len(itemLines) > 0 {
			footerLines = append(footerLines, "")
		}
		footerLines = append(footerLines, c.footer()...)
	}
	lines := clampRows(itemLines, footerLines, c.rowBudget())
	budget := c.budget()
	for i, s := range lines {
		if i > 0 {
			_, _ = io.WriteString(c.w, "\n")
		}
		_, _ = io.WriteString(c.w, clampANSI(s, budget))
	}
	c.shown = len(lines)
}

// clampRows bounds the block to maxRows total lines. The footer (the run
// summary) is always kept; only the item lines are trimmed, with a dim marker
// standing in for what was dropped — so many concurrent groups can never grow
// the block past the screen and break the cursor-up erase math.
func clampRows(itemLines, footerLines []string, maxRows int) []string {
	total := len(itemLines) + len(footerLines)
	if total <= maxRows {
		return append(itemLines, footerLines...)
	}
	// Reserve the footer plus one row for the elision marker; show as many of the
	// leading item lines as the rest allows (possibly none).
	keep := maxRows - len(footerLines) - 1
	if keep < 0 {
		keep = 0
	}
	if keep > len(itemLines) {
		keep = len(itemLines)
	}
	dropped := len(itemLines) - keep
	out := make([]string, 0, keep+1+len(footerLines))
	out = append(out, itemLines[:keep]...)
	out = append(out, fmt.Sprintf("    … %d more", dropped))
	out = append(out, footerLines...)
	return out
}

func (c *console) ensureTicker() {
	if c.ticker != nil {
		return
	}
	c.ticker = time.NewTicker(100 * time.Millisecond)
	c.stop = make(chan struct{})
	// Pass the ticker/stop in so the goroutine holds its own references even
	// after stopTicker nils the fields for the next run.
	go c.animate(c.ticker, c.stop)
}

func (c *console) stopTicker() {
	if c.ticker == nil {
		return
	}
	c.ticker.Stop()
	close(c.stop)
	c.ticker, c.stop = nil, nil
}

func (c *console) animate(tk *time.Ticker, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			c.mu.Lock()
			c.frame++
			c.eraseBlock()
			c.drawBlock()
			c.mu.Unlock()
		}
	}
}

// WriteLine writes one complete line to w (newline included) as a section of the
// given kind, coordinated with any in-flight progress so status lines print
// cleanly above the live block and consecutive sections stay one blank line apart.
// The command layer uses it for the plan and run-summary lines it prints while
// pipelines may be animating.
func WriteLine(kind Section, w io.Writer, s string) { liveTerm.line(kind, w, s) }
