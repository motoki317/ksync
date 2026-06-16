package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// PromptItem is one selectable change shown in the watch confirmation picker: a
// dirty image (Icon = IconBuild, Label = build name, Note = its app) or a
// manifest-only redeploy (Icon = IconDeploy, Label = app, Note = "manifests only").
type PromptItem struct {
	Icon  string
	Label string
	Note  string
}

// keyEvent is one decoded keystroke the picker model understands; the raw-mode
// driver translates terminal bytes (arrow escape sequences, space, …) into these
// so the model stays free of any terminal specifics and is unit-testable.
type keyEvent int

const (
	keyNone keyEvent = iota
	keyUp
	keyDown
	keySpace
	keyEnter
	keyAll  // toggle every checkbox (the 'a' key)
	keyQuit // q / Ctrl-C: abort the whole watch
)

// The top single-select menu, shown first. The cursor defaults to Build all, so
// a developer who just wants everything rebuilt confirms with one Enter.
const (
	topBuildAll = iota
	topSelect
	topSkip
)

var topMenu = []string{"Build all", "Select which to build", "Skip"}

// buildPrompt is the pure state machine behind the two-step confirmation picker:
// a top menu (Build all · Select which to build · Skip), then — only if Select —
// an arrow-key, space-to-toggle multi-select over the changed items. It holds no
// terminal state; handle() advances it from a keyEvent and render() returns the
// lines to paint, so the decision logic is tested without a real terminal.
type buildPrompt struct {
	c     Colors
	items []PromptItem

	step      int // 0 = top menu, 1 = multi-select
	topCursor int
	cursor    int
	checked   []bool

	done  bool
	build bool // act on selected() (false = skip, build nothing)
	quit  bool // abort the watch entirely
}

func newBuildPrompt(c Colors, items []PromptItem) *buildPrompt {
	return &buildPrompt{c: c, items: items, checked: make([]bool, len(items))}
}

// handle advances the model by one keystroke.
func (m *buildPrompt) handle(k keyEvent) {
	if k == keyQuit {
		m.quit, m.done = true, true
		return
	}
	if m.step == 0 {
		m.handleMenu(k)
		return
	}
	m.handleSelect(k)
}

func (m *buildPrompt) handleMenu(k keyEvent) {
	switch k {
	case keyUp:
		if m.topCursor > 0 {
			m.topCursor--
		}
	case keyDown:
		if m.topCursor < len(topMenu)-1 {
			m.topCursor++
		}
	case keyEnter:
		switch m.topCursor {
		case topBuildAll:
			for i := range m.checked {
				m.checked[i] = true
			}
			m.build, m.done = true, true
		case topSelect:
			m.step = 1 // start with nothing checked: the developer picks the subset
		case topSkip:
			m.done = true
		}
	}
}

func (m *buildPrompt) handleSelect(k keyEvent) {
	switch k {
	case keyUp:
		if m.cursor > 0 {
			m.cursor--
		}
	case keyDown:
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case keySpace:
		if len(m.checked) > 0 {
			m.checked[m.cursor] = !m.checked[m.cursor]
		}
	case keyAll:
		all := true
		for _, ck := range m.checked {
			all = all && ck
		}
		for i := range m.checked {
			m.checked[i] = !all // all checked → clear; otherwise check every one
		}
	case keyEnter:
		// Confirming with nothing checked means "build nothing" — the same outcome
		// as Skip, so an empty selection never starts a no-op build.
		m.build = m.anyChecked()
		m.done = true
	}
}

func (m *buildPrompt) anyChecked() bool {
	for _, ck := range m.checked {
		if ck {
			return true
		}
	}
	return false
}

// selected returns the indices of the checked items (every item for Build all,
// which checks them all). Empty when the user skipped.
func (m *buildPrompt) selected() []int {
	var out []int
	for i, ck := range m.checked {
		if ck {
			out = append(out, i)
		}
	}
	return out
}

// render returns the picker's current lines (no trailing newlines); the driver
// clamps and paints them.
func (m *buildPrompt) render() []string {
	if m.step == 0 {
		return m.renderMenu()
	}
	return m.renderSelect()
}

func (m *buildPrompt) renderMenu() []string {
	plural := "s"
	if len(m.items) == 1 {
		plural = ""
	}
	lines := []string{
		"",
		m.c.Bold(fmt.Sprintf("%d change%s pending", len(m.items), plural)) + m.c.Dim(" — rebuild & deploy?"),
		"",
	}
	for i, opt := range topMenu {
		if i == m.topCursor {
			lines = append(lines, m.c.Cyan("❯ ")+m.c.Bold(opt))
		} else {
			lines = append(lines, "  "+opt)
		}
	}
	return append(lines, "", m.c.Dim("↑/↓ move · Enter confirm · q quit"))
}

func (m *buildPrompt) renderSelect() []string {
	lines := []string{
		"",
		m.c.Bold("Select images to rebuild & deploy") + "  " + m.c.Dim("Space toggle · a all · Enter confirm"),
		"",
	}
	labelW := 0
	for _, it := range m.items {
		if w := displayWidth(it.Label); w > labelW {
			labelW = w
		}
	}
	for i, it := range m.items {
		box := m.c.Dim("◯")
		if m.checked[i] {
			box = m.c.Green("◉")
		}
		prefix := "  "
		if i == m.cursor {
			prefix = m.c.Cyan("❯ ")
		}
		label := it.Label + strings.Repeat(" ", labelW-displayWidth(it.Label))
		row := fmt.Sprintf("%s%s %s %s  %s", prefix, box, it.Icon, label, m.c.Dim(it.Note))
		lines = append(lines, strings.TrimRight(row, " "))
	}
	return append(lines, "", m.c.Dim("↑/↓ move · q quit"))
}

// ConfirmBuilds runs the two-step interactive gate on the terminal (reading raw
// keystrokes from in, painting to out): a Build all / Select / Skip menu with the
// cursor on Build all, then — only if Select — an arrow-key, space-to-toggle
// multi-select over items. It returns the chosen item indices, whether to build
// anything (false = skip), and whether the user asked to quit ksync (Ctrl-C / q).
// ctx cancellation (e.g. SIGTERM) aborts as a quit. The block is erased before
// returning, so it leaves no trace behind the build output or the next prompt.
// The caller guarantees in is a terminal and items is non-empty.
func ConfirmBuilds(ctx context.Context, in *os.File, out io.Writer, c Colors, items []PromptItem) (selected []int, build, quit bool) {
	m := newBuildPrompt(c, items)
	fd := int(in.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		// Cannot take the terminal — fail safe to Build all rather than silently
		// dropping the changes the developer is waiting on.
		return allIndices(len(items)), true, false
	}
	defer func() { _ = term.Restore(fd, old) }()

	// Discard anything typed before the prompt opened — a stray Enter or arrow the
	// developer hit while the loop sat idle in cooked mode is queued on stdin, and
	// the first raw read would otherwise consume it and (an Enter on the default
	// Build-all cursor) confirm the prompt before it is even seen.
	flushInput(in)

	budget := cols(fd) - liveMargin
	shown := 0
	paint := func() {
		lines := m.render()
		var b strings.Builder
		b.WriteString(erasePrev(shown))
		for i, ln := range lines {
			if i > 0 {
				b.WriteString("\r\n") // raw mode: no output post-processing, so \n needs an explicit \r
			}
			b.WriteString(clampANSI(ln, budget))
		}
		_, _ = io.WriteString(out, b.String())
		shown = len(lines)
	}

	paint()
	buf := make([]byte, 32)
	for !m.done {
		_ = in.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, rerr := in.Read(buf)
		if rerr != nil {
			if os.IsTimeout(rerr) {
				if ctx.Err() != nil {
					m.quit, m.done = true, true
					break
				}
				continue
			}
			m.quit, m.done = true, true // EOF or read error: abort safely
			break
		}
		for _, k := range decodeKeys(buf[:n]) {
			m.handle(k)
			if m.done {
				break
			}
		}
		if !m.done {
			paint()
		}
	}
	_, _ = io.WriteString(out, erasePrev(shown))
	if m.quit {
		return nil, false, true
	}
	return m.selected(), m.build, false
}

// erasePrev returns the control sequence that clears the n lines just painted,
// leaving the cursor at column 0 of where the block's top line was — the same
// cursor-up-and-erase accounting the live block uses.
func erasePrev(n int) string {
	if n == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(eraseLine) // clear the last (cursor's) line
	for i := 1; i < n; i++ {
		b.WriteString(cursorUp + eraseLine)
	}
	return b.String()
}

// flushDrain bounds how long flushInput waits to confirm the input queue is
// empty. Already-buffered bytes are returned immediately (the deadline only
// trips when a read would block), so this is a one-time cost paid once at
// prompt-open with no buffered input — imperceptible to the developer.
const flushDrain = 20 * time.Millisecond

// flushInput drains and discards input already queued on in before the picker
// took the terminal. It reads under a short future deadline (not a past one,
// which can race the deadline timer and skip buffered bytes) until a read would
// block, throwing away whatever it finds. Real terminals support read deadlines;
// if in does not, the first read simply blocks until a keypress — no worse than
// the read loop that follows, which depends on the same mechanism.
func flushInput(in *os.File) {
	buf := make([]byte, 256)
	for {
		_ = in.SetReadDeadline(time.Now().Add(flushDrain))
		n, err := in.Read(buf)
		if err != nil || n == 0 {
			return
		}
	}
}

func allIndices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// decodeKeys turns a slice of raw terminal bytes into key events, recognizing the
// arrow escape sequences (ESC [ A/B), Enter, Space, 'a', vim j/k, and the quit
// keys (q, Ctrl-C, Ctrl-D). Unknown bytes are ignored.
func decodeKeys(b []byte) []keyEvent {
	var keys []keyEvent
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case 0x1b: // ESC: an arrow sequence "ESC [ A".."D", or a lone ESC (ignored)
			if i+2 < len(b) && b[i+1] == '[' {
				switch b[i+2] {
				case 'A':
					keys = append(keys, keyUp)
				case 'B':
					keys = append(keys, keyDown)
				}
				i += 2 // consume the whole sequence (C/D and others fall through as ignored)
			}
		case '\r', '\n':
			keys = append(keys, keyEnter)
		case ' ':
			keys = append(keys, keySpace)
		case 'a', 'A':
			keys = append(keys, keyAll)
		case 'k':
			keys = append(keys, keyUp)
		case 'j':
			keys = append(keys, keyDown)
		case 'q', 'Q', 0x03, 0x04: // q, Ctrl-C, Ctrl-D
			keys = append(keys, keyQuit)
		}
	}
	return keys
}
