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

// The top menu starts on Build all, so a single Enter selects every change.
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

// Skip (third menu row) finishes without building anything.
func TestBuildPrompt_SkipBuildsNothing(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	feed(m, keyDown, keyDown, keyEnter) // Build all → Select → Skip
	if !m.done || m.build {
		t.Fatalf("Skip should finish without building, got done=%v build=%v", m.done, m.build)
	}
	if got := m.selected(); len(got) != 0 {
		t.Errorf("Skip should select nothing, got %v", got)
	}
}

// "Select which to build" opens the multi-select with nothing checked; Space
// toggles individual rows and Enter confirms exactly that subset.
func TestBuildPrompt_SelectSubset(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	// Build all → Select, enter multi-select.
	feed(m, keyDown, keyEnter)
	if m.step != 1 {
		t.Fatalf("Select should enter the multi-select step, got step=%d", m.step)
	}
	// Check item 0 (cursor starts there), move to item 2 and check it; leave 1 off.
	feed(m, keySpace, keyDown, keyDown, keySpace, keyEnter)
	if !m.done || !m.build {
		t.Fatalf("a non-empty subset should build, got done=%v build=%v", m.done, m.build)
	}
	if got := m.selected(); !slices.Equal(got, []int{0, 2}) {
		t.Errorf("selected = %v, want [0 2] (the two toggled rows)", got)
	}
}

// In the multi-select, 'a' checks all when any is unchecked and clears all when
// every row is already checked.
func TestBuildPrompt_ToggleAll(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	feed(m, keyDown, keyEnter, keyAll) // into multi-select, then select all
	if got := m.selected(); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("'a' should check all, got %v", got)
	}
	feed(m, keyAll) // toggle again clears all
	if got := m.selected(); len(got) != 0 {
		t.Errorf("'a' on an all-checked list should clear it, got %v", got)
	}
}

// Confirming the multi-select with nothing checked is the same as Skip.
func TestBuildPrompt_EmptySelectionIsSkip(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	feed(m, keyDown, keyEnter, keyEnter) // Select, then confirm with none checked
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

// The menu marks the cursor row and lists every option; the multi-select shows
// each item's icon, label, and note.
func TestBuildPrompt_Render(t *testing.T) {
	m := newBuildPrompt(Colors{}, items3())
	menu := strings.Join(m.render(), "\n")
	if !strings.Contains(menu, "❯ Build all") {
		t.Errorf("menu should point the cursor at Build all, got:\n%s", menu)
	}
	for _, want := range []string{"Select which to build", "Skip", "3 changes pending"} {
		if !strings.Contains(menu, want) {
			t.Errorf("menu missing %q, got:\n%s", want, menu)
		}
	}
	feed(m, keyDown, keyEnter) // into multi-select
	sel := strings.Join(m.render(), "\n")
	for _, want := range []string{IconBuild, "web", "worker", IconDeploy, "shop", "manifests only", "◯"} {
		if !strings.Contains(sel, want) {
			t.Errorf("multi-select missing %q, got:\n%s", want, sel)
		}
	}
	feed(m, keySpace) // check the cursor row
	if checked := strings.Join(m.render(), "\n"); !strings.Contains(checked, "◉") {
		t.Errorf("a checked row should show ◉, got:\n%s", checked)
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
	sel, build, quit := ConfirmBuilds(context.Background(), r, &out, Colors{}, items3())
	if !build || quit {
		t.Fatalf("non-terminal should fall back to build-all, got build=%v quit=%v", build, quit)
	}
	if !slices.Equal(sel, []int{0, 1, 2}) {
		t.Errorf("fallback should select every item, got %v", sel)
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
		{"q", []byte{'q'}, []keyEvent{keyQuit}},
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
