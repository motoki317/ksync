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
	keyQuit // Ctrl-C / Ctrl-D: abort the whole watch
)

// buildPrompt is the pure state machine behind the single-view confirmation
// picker: one "Rebuild all" master checkbox above one row per changed item. The
// cursor defaults to the master with all on, so a developer who wants everything
// rebuilt confirms with one Enter; toggling an item narrows the selection to just
// the picked subset. It holds no terminal state — handle() advances it from a
// keyEvent and render() returns the lines to paint — so the decision logic is
// tested without a real terminal.
//
// Selection is a mode, not a tri-state: `all` true means every item (rendered
// implied), and toggling an item flips to an explicit `checked` set. This is what
// lets Space on an item while "Rebuild all" is on mean "only this one" rather than
// "all but this one" — the developer who wants a single image picks it directly.
type buildPrompt struct {
	c     Colors
	items []PromptItem

	cursor  int    // 0 = the Rebuild-all master, 1..len(items) = item rows
	all     bool   // every item selected via the master (checked is then ignored)
	checked []bool // the explicit per-item set, meaningful only when all is false

	done  bool
	build bool // act on selected() (false = skip, build nothing)
	quit  bool // abort the watch entirely
}

func newBuildPrompt(c Colors, items []PromptItem) *buildPrompt {
	return &buildPrompt{c: c, items: items, all: true, checked: make([]bool, len(items))}
}

// handle advances the model by one keystroke. The cursor ranges over row 0 (the
// Rebuild-all master) through len(items); Space toggles the row under the cursor
// and Enter confirms the current selection — building all, the chosen subset, or
// (nothing selected) nothing at all.
func (m *buildPrompt) handle(k keyEvent) {
	switch k {
	case keyQuit:
		m.quit, m.done = true, true
	case keyUp:
		if m.cursor > 0 {
			m.cursor--
		}
	case keyDown:
		if m.cursor < len(m.items) {
			m.cursor++
		}
	case keySpace:
		m.toggle(m.cursor)
	case keyAll:
		m.setAll(!m.all) // a convenience equal to Space on the master row
	case keyEnter:
		// Confirming with nothing selected means "build nothing" — the skip path now
		// that the menu's Skip row is gone, reached by clearing the master (or every
		// item) before Enter. An empty selection never starts a no-op build.
		m.build = m.all || m.anyChecked()
		m.done = true
	}
}

// toggle flips the checkbox at the cursor row. Toggling the master (row 0) switches
// between all-selected and the empty, skip-ready set. Toggling an item while the
// master is on narrows the selection to just that item, so picking one image never
// requires first clearing the rest.
func (m *buildPrompt) toggle(row int) {
	if row == 0 {
		m.setAll(!m.all)
		return
	}
	i := row - 1
	if m.all {
		m.setAll(false)
		m.checked[i] = true
		return
	}
	m.checked[i] = !m.checked[i]
}

// setAll switches the master on or off; turning it off clears the explicit set so
// the off state is unambiguously empty (the skip-ready state).
func (m *buildPrompt) setAll(on bool) {
	m.all = on
	if !on {
		for i := range m.checked {
			m.checked[i] = false
		}
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

// selected returns the indices to build: every item when the master is on,
// otherwise the explicitly checked ones. Empty when the user skipped.
func (m *buildPrompt) selected() []int {
	var out []int
	for i := range m.items {
		if m.all || m.checked[i] {
			out = append(out, i)
		}
	}
	return out
}

// render returns the picker's current lines (no trailing newlines); the driver
// clamps and paints them. The single view is the Rebuild-all master over one row
// per item — items shown dim/implied while the master is on, carrying their own
// checkbox once an individual pick turns the master off.
func (m *buildPrompt) render() []string {
	plural := "s"
	if len(m.items) == 1 {
		plural = ""
	}
	lines := []string{
		"",
		m.c.Bold(fmt.Sprintf("%d change%s pending", len(m.items), plural)) + m.c.Dim(" — rebuild & deploy?"),
		"",
		m.prefix(0) + m.box(m.all, false) + " " + m.c.Bold("Rebuild all"),
	}
	labelW := 0
	for _, it := range m.items {
		if w := displayWidth(it.Label); w > labelW {
			labelW = w
		}
	}
	for i, it := range m.items {
		box := m.box(m.all || m.checked[i], m.all) // dim/implied while the master is on
		label := it.Label + strings.Repeat(" ", labelW-displayWidth(it.Label))
		body := fmt.Sprintf("%s %s  %s", it.Icon, label, it.Note)
		if m.all {
			body = m.c.Dim(body) // the master owns these rows; render them passive
		} else {
			body = fmt.Sprintf("%s %s  %s", it.Icon, label, m.c.Dim(it.Note))
		}
		row := m.prefix(i+1) + "  " + box + " " + body
		lines = append(lines, strings.TrimRight(row, " "))
	}
	return append(lines, "", m.c.Dim("↑/↓ move · Space toggle · Enter confirm"))
}

// prefix is the cursor marker for a row, indenting non-cursor rows to align.
func (m *buildPrompt) prefix(row int) string {
	if row == m.cursor {
		return m.c.Cyan("❯ ")
	}
	return "  "
}

// box renders a checkbox: a filled circle when selected (dim when only implied by
// the master, green when explicitly chosen), a dim hollow circle when not.
func (m *buildPrompt) box(filled, implied bool) string {
	if !filled {
		return m.c.Dim("◯")
	}
	if implied {
		return m.c.Dim("◉")
	}
	return m.c.Green("◉")
}

// pollInterval is how often the picker re-reads the terminal for keystrokes while
// idle (no key queued), between which it also checks ctx and the abort signal. Far
// below human reaction time, so navigation feels instant; the loop is otherwise
// asleep, so the cost is negligible.
const pollInterval = 25 * time.Millisecond

// ConfirmBuilds runs the single-view interactive gate on the terminal (reading raw
// keystrokes from in, painting to out): a Rebuild-all master checkbox (cursor
// default, on) over one space-to-toggle row per item, where toggling an item
// narrows to that subset. It returns the chosen item indices, whether to build
// anything (false = skip, reached by clearing the selection), whether the user
// asked to quit ksync (Ctrl-C), and whether the prompt was aborted by the loop
// (abort closed — fresh changes arrived, so the loop will re-ask with the larger
// set). The four outcomes are mutually exclusive; an aborted prompt acted on
// nothing. ctx cancellation (e.g. SIGTERM) and abort are honored within
// pollInterval on a terminal (the read fd is polled non-blocking, since no tty
// supports read deadlines); off unix the read blocks, so both are honored only
// once a key is pressed — Ctrl-C is itself a keystroke (raw mode delivers no
// SIGINT) and always works. The block is erased before returning, so it leaves no
// trace behind the build output or the next prompt. in is guaranteed a terminal,
// items non-empty.
func ConfirmBuilds(ctx context.Context, abort <-chan struct{}, in *os.File, out io.Writer, c Colors, items []PromptItem) (selected []int, build, quit, aborted bool) {
	m := newBuildPrompt(c, items)
	fd := int(in.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		// Cannot take the terminal — fail safe to Build all rather than silently
		// dropping the changes the developer is waiting on.
		return allIndices(len(items)), true, false, false
	}
	defer func() { _ = term.Restore(fd, old) }()

	// Discard anything typed before the prompt opened — a stray Enter or arrow the
	// developer hit while the loop sat idle in cooked mode is queued on stdin, and
	// the first raw read would otherwise consume it and (an Enter on the default
	// Rebuild-all selection) confirm the prompt before it is even seen.
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
	if read, restore, perr := pollReader(fd); perr == nil {
		defer restore() // back to blocking before term.Restore; never leak O_NONBLOCK
		aborted = pollLoop(ctx, abort, read, m, paint)
	} else {
		aborted = blockingLoop(ctx, abort, in, m, paint)
	}
	_, _ = io.WriteString(out, erasePrev(shown))
	switch {
	case aborted:
		return nil, false, false, true
	case m.quit:
		return nil, false, true, false
	default:
		return m.selected(), m.build, false, false
	}
}

// pollLoop drives the picker on a terminal: poll the non-blocking fd for keys and,
// when nothing is queued, wait one tick while watching ctx and abort. Returns true
// when abort fired (the loop wants to re-ask), leaving m untouched.
func pollLoop(ctx context.Context, abort <-chan struct{}, read func([]byte) (int, error), m *buildPrompt, paint func()) bool {
	buf := make([]byte, 32)
	for !m.done {
		n, rerr := read(buf)
		if rerr != nil {
			m.quit, m.done = true, true // EOF or read error: abort safely
			break
		}
		if n > 0 {
			for _, k := range decodeKeys(buf[:n]) {
				m.handle(k)
				if m.done {
					break
				}
			}
			if !m.done {
				paint()
			}
			continue // drain a key burst before sleeping
		}
		select {
		case <-ctx.Done():
			m.quit, m.done = true, true
		case <-abort:
			return true
		case <-time.After(pollInterval):
		}
	}
	return false
}

// blockingLoop drives the picker where non-blocking polling is unavailable (off
// unix): a plain blocking read, with ctx and abort honored only once a key arrives
// (the read cannot be interrupted there). Returns true when abort fired.
func blockingLoop(ctx context.Context, abort <-chan struct{}, in *os.File, m *buildPrompt, paint func()) bool {
	buf := make([]byte, 32)
	for !m.done {
		n, rerr := in.Read(buf)
		if rerr != nil {
			m.quit, m.done = true, true // EOF or read error: abort safely
			break
		}
		select {
		case <-ctx.Done():
			m.quit, m.done = true, true
		case <-abort:
			return true
		default:
		}
		if m.done {
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
	return false
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

// flushDrain bounds how long the deadline-based branch of flushInput waits to
// confirm the input queue is empty. Already-buffered bytes are returned
// immediately (the deadline only trips when a read would block), so this is a
// one-time cost paid once at prompt-open with no buffered input — imperceptible.
const flushDrain = 20 * time.Millisecond

// flushInput drains and discards input already queued on in before the picker took
// the terminal, so a stray Enter or arrow typed while the loop sat idle in cooked
// mode cannot confirm the default the instant the prompt opens.
//
// os.File read deadlines are not universal: a real terminal — notably every macOS
// tty — reports "file type does not support deadline" from SetReadDeadline. With no
// deadline, in.Read blocks until a keypress, so a drain loop that relies on it hangs
// forever and swallows every keystroke, Ctrl-C included (the freeze this fixes). So
// probe the deadline first; when it is unsupported, fall back to a non-blocking
// syscall drain that returns immediately on an empty queue. (The main read loop
// below still uses the deadline only as a best-effort wakeup; without it a blocked
// read wakes on the next keypress, which is harmless for an idle prompt.)
func flushInput(in *os.File) {
	if err := in.SetReadDeadline(time.Now().Add(flushDrain)); err != nil {
		drainTTY(in)
		return
	}
	buf := make([]byte, 256)
	for {
		n, err := in.Read(buf)
		if err != nil || n == 0 {
			_ = in.SetReadDeadline(time.Time{}) // clear the drain deadline
			return
		}
		_ = in.SetReadDeadline(time.Now().Add(flushDrain))
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
// keys (Ctrl-C, Ctrl-D). Unknown bytes are ignored. 'q' is intentionally not a
// quit key — it is a plausible future filter keystroke, and Ctrl-C already aborts.
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
		case 0x03, 0x04: // Ctrl-C, Ctrl-D
			keys = append(keys, keyQuit)
		}
	}
	return keys
}
