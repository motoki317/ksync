package ui

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func items3() []PromptItem {
	return []PromptItem{
		{Icon: IconBuild, Label: "web", Note: "api-b"},
		{Icon: IconBuild, Label: "worker", Note: "api-b"},
		{Icon: IconDeploy, Label: "shop", Note: "manifests only"},
	}
}

func feed(m *buildPrompt, keys ...keyEvent) {
	for _, k := range keys {
		m.handle(k)
	}
}

// The cursor starts on the Rebuild-all master with all selected, so a single Enter
// selects every change.
func TestBuildPrompt_BuildAllIsTheDefault(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	feed(m, keyEnter)
	if !m.done || !m.build {
		t.Fatalf("Enter on the default should build, got done=%v build=%v", m.done, m.build)
	}
	if got := m.selected(); !slices.Equal(got, []int{0, 1, 2}) {
		t.Errorf("Build all should select every item, got %v", got)
	}
}

// Clearing the master (Space on the default row) leaves nothing selected, so the
// next Enter skips — the two-key skip that replaces the removed Skip menu row.
func TestBuildPrompt_SkipBuildsNothing(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	feed(m, keySpace, keyEnter) // Space on Rebuild all (→ off), then confirm
	if !m.done || m.build {
		t.Fatalf("Skip should finish without building, got done=%v build=%v", m.done, m.build)
	}
	if got := m.selected(); len(got) != 0 {
		t.Errorf("Skip should select nothing, got %v", got)
	}
}

// Toggling an item while the master is on narrows the selection to just that item;
// further Space toggles are additive, and Enter confirms exactly that subset.
func TestBuildPrompt_SelectSubset(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	// Down to item 0 and pick it (turns the master off, leaving only item 0), then
	// down to item 2 and add it; leave item 1 off.
	feed(m, keyDown, keySpace, keyDown, keyDown, keySpace, keyEnter)
	if !m.done || !m.build {
		t.Fatalf("a non-empty subset should build, got done=%v build=%v", m.done, m.build)
	}
	if m.all {
		t.Errorf("picking an item should turn the master off")
	}
	if got := m.selected(); !slices.Equal(got, []int{0, 2}) {
		t.Errorf("selected = %v, want [0 2] (the two toggled rows)", got)
	}
}

// 'a' toggles the master like Space on its row: from the default (all on) it clears
// to nothing, and again restores all.
func TestBuildPrompt_ToggleAll(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	feed(m, keyAll) // all on → off
	if got := m.selected(); len(got) != 0 {
		t.Fatalf("'a' on the default (all on) should clear it, got %v", got)
	}
	feed(m, keyAll) // off → all
	if got := m.selected(); !slices.Equal(got, []int{0, 1, 2}) {
		t.Errorf("'a' should restore all, got %v", got)
	}
}

// Toggling an item on and back off leaves an empty explicit set, so Enter skips.
func TestBuildPrompt_EmptySelectionIsSkip(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	feed(m, keyDown, keySpace, keySpace, keyEnter) // pick item 0, unpick it, confirm
	if !m.done || m.build {
		t.Errorf("an empty selection must not build, got done=%v build=%v", m.done, m.build)
	}
}

// Quit aborts from either step.
func TestBuildPrompt_Quit(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	feed(m, keyQuit)
	if !m.done || !m.quit {
		t.Errorf("quit should finish with quit set, got done=%v quit=%v", m.done, m.quit)
	}
}

// The single view points the cursor at the Rebuild-all master and lists every
// item's icon, label, and note below it; turning the master off shows a hollow box
// for an unpicked row and a filled one for the picked row.
func TestBuildPrompt_Render(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	view := strings.Join(m.render(), "\n")
	if !strings.Contains(view, "❯ ◉ Rebuild all") {
		t.Errorf("view should point the cursor at the Rebuild-all master, got:\n%s", view)
	}
	for _, want := range []string{"3 changes pending", IconBuild, "web", "worker", IconDeploy, "shop", "manifests only"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q, got:\n%s", want, view)
		}
	}
	feed(m, keyDown, keySpace) // pick item 0: master off, item 0 filled, others hollow
	picked := strings.Join(m.render(), "\n")
	for _, want := range []string{"◯", "◉"} {
		if !strings.Contains(picked, want) {
			t.Errorf("explicit selection missing %q, got:\n%s", want, picked)
		}
	}
}

// erasePrev clears exactly the n lines just painted: clear the current line,
// then move up and clear for each line above it.
func TestErasePrev(t *testing.T) {
	if got := erasePrev(0); got != "" {
		t.Errorf("erasePrev(0) = %q, want empty", got)
	}
	if got := erasePrev(1); got != eraseLine {
		t.Errorf("erasePrev(1) = %q, want one eraseLine", got)
	}
	if got, want := erasePrev(3), eraseLine+cursorUp+eraseLine+cursorUp+eraseLine; got != want {
		t.Errorf("erasePrev(3) = %q, want %q", got, want)
	}
}

// When stdin is not a terminal, raw mode cannot be entered, so the picker fails
// safe to Build all rather than silently dropping the pending changes.
func TestConfirmBuilds_NonTerminalFallsBackToBuildAll(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	var out bytes.Buffer
	sel, build, quit, aborted := ConfirmBuilds(context.Background(), nil, r, &out, Colors{}, items3())
	if !build || quit || aborted {
		t.Fatalf("non-terminal should fall back to build-all, got build=%v quit=%v aborted=%v", build, quit, aborted)
	}
	if !slices.Equal(sel, []int{0, 1, 2}) {
		t.Errorf("fallback should select every item, got %v", sel)
	}
}

// noData is a read that never yields a key, so pollLoop reaches its idle select
// each iteration — the path where ctx and abort are honored on a real terminal.
func noData([]byte) (int, error) { return 0, nil }

// A closed abort channel makes pollLoop return aborted without any keystroke — the
// crux of the fix: a prompt left open on a tty refreshes when fresh changes land,
// which on a tty (no read deadline) can only happen between polls, not on a read.
func TestPollLoop_AbortReturnsWithoutKeystroke(t *testing.T) {
	abort := make(chan struct{})
	close(abort)
	m := newBuildPrompt(Colors{}, items3())
	if got := pollLoop(context.Background(), abort, noData, m, func() {}); !got {
		t.Fatalf("closed abort should return aborted=true")
	}
	if m.done {
		t.Errorf("an aborted prompt must not be marked done/decided")
	}
}

// ctx cancellation ends the prompt as a quit (not an abort), again without a key —
// so SIGTERM during an idle prompt is honored within a poll tick on a tty.
func TestPollLoop_CtxCancelQuits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := newBuildPrompt(Colors{}, items3())
	if got := pollLoop(ctx, make(chan struct{}), noData, m, func() {}); got {
		t.Errorf("ctx cancel is a quit, not an abort")
	}
	if !m.quit || !m.done {
		t.Errorf("ctx cancel should quit, got quit=%v done=%v", m.quit, m.done)
	}
}

// A keystroke read through pollLoop drives the model: Enter on the default confirms
// build-all, and the loop returns not-aborted.
func TestPollLoop_HandlesKeystroke(t *testing.T) {
	sent := false
	read := func(buf []byte) (int, error) {
		if !sent {
			sent = true
			return copy(buf, []byte{'\r'}), nil // Enter
		}
		return 0, nil
	}
	m := newBuildPrompt(Colors{}, items3())
	if got := pollLoop(context.Background(), make(chan struct{}), read, m, func() {}); got {
		t.Errorf("a confirm is not an abort")
	}
	if !m.done || !m.build {
		t.Errorf("Enter on the default should confirm build-all, got done=%v build=%v", m.done, m.build)
	}
}

// pollReader yields queued bytes and reports an empty queue as (0, nil) rather than
// blocking — the non-blocking read the poll loop needs where no tty supports a read
// deadline. Tested on a pipe (where SetNonblock works) so it needs no real tty.
func TestPollReader_NonBlocking(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	read, restore, err := pollReader(int(r.Fd()))
	if err != nil {
		t.Skipf("non-blocking reads unsupported on this platform: %v", err)
	}
	defer restore()

	buf := make([]byte, 16)
	if n, err := read(buf); n != 0 || err != nil {
		t.Errorf("empty queue should read (0, nil), got n=%d err=%v", n, err)
	}
	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	// The byte may not be visible on the very next syscall; poll briefly.
	var got string
	for i := 0; i < 100 && got == ""; i++ {
		if n, err := read(buf); err != nil {
			t.Fatalf("read after write: %v", err)
		} else if n > 0 {
			got = string(buf[:n])
		} else {
			time.Sleep(time.Millisecond)
		}
	}
	if got != "hi" {
		t.Errorf("queued bytes should read back, got %q", got)
	}
}

// flushInput must discard keystrokes queued before the picker opened, so a stray
// Enter pressed while the loop was idle cannot confirm the default unseen.
func TestFlushInput_DiscardsBufferedBytes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	if err := r.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Skipf("read deadlines unsupported on this platform's pipes: %v", err)
	}
	_ = r.SetReadDeadline(time.Time{}) // clear the probe deadline

	// Two stray Enters and a down-arrow, as if typed while the loop sat idle.
	if _, err := w.Write([]byte("\r\r\x1b[B")); err != nil {
		t.Fatal(err)
	}
	flushInput(r)

	// Nothing should remain: a read with a short deadline trips the deadline.
	_ = r.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	n, err := r.Read(make([]byte, 16))
	if n != 0 || !os.IsTimeout(err) {
		t.Fatalf("after flush, read got %d byte(s) (err=%v), want an empty queue", n, err)
	}
}

func TestDecodeKeys(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []keyEvent
	}{
		{"arrow up", []byte{0x1b, '[', 'A'}, []keyEvent{keyUp}},
		{"arrow down", []byte{0x1b, '[', 'B'}, []keyEvent{keyDown}},
		{"space", []byte{' '}, []keyEvent{keySpace}},
		{"enter CR", []byte{'\r'}, []keyEvent{keyEnter}},
		{"enter LF", []byte{'\n'}, []keyEvent{keyEnter}},
		{"toggle all", []byte{'a'}, []keyEvent{keyAll}},
		{"vim down/up", []byte{'j', 'k'}, []keyEvent{keyDown, keyUp}},
		{"ctrl-c", []byte{0x03}, []keyEvent{keyQuit}},
		{"q is not quit", []byte{'q'}, nil},
		{"two arrows in one read", []byte{0x1b, '[', 'B', 0x1b, '[', 'B'}, []keyEvent{keyDown, keyDown}},
		{"ignored bytes", []byte{'z', '5'}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeKeys(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("decodeKeys(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
