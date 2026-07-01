package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// cmdListHint is the next-step nudge exitStatus appends to a genuinely-unknown
// top-level verb. cobra.NoArgs formats extra args on a real command with the same
// `unknown command %q for %q` shape, so the hint must be anchored to the root path,
// not the bare prefix — these tests guard that distinction.
const cmdListHint = "run 'ksync help' for the command list"

// TestExitStatus_UnknownCommandHint checks that the command-list hint fires only for
// a wrong verb (`ksync frob`), not for extra args on a real command (`ksync version
// extra`) or a bad flag — cases whose real fix the command list does not point at.
func TestExitStatus_UnknownCommandHint(t *testing.T) {
	root := newRootCmd()
	cases := []struct {
		name     string
		args     []string
		wantHint bool
		wantSub  string // the mapped message must always contain this
	}{
		{"unknown top-level verb", []string{"frob"}, true, `unknown command "frob"`},
		{"extra args on a leaf command", []string{"version", "extra"}, false, `unknown command "extra"`},
		{"unknown flag", []string{"sync", "--bogus"}, false, "unknown flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := executeKsync(t, tc.args...)
			if err == nil {
				t.Fatalf("%v: expected an error", tc.args)
			}
			msg, code := exitStatus(err, root)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(msg, tc.wantSub) {
				t.Errorf("message %q does not contain %q", msg, tc.wantSub)
			}
			if got := strings.Contains(msg, cmdListHint); got != tc.wantHint {
				t.Errorf("command-list hint present = %v, want %v (message: %q)", got, tc.wantHint, msg)
			}
		})
	}
}

// TestExitStatus_InterruptAndSuccess covers the non-error and cancellation arms: nil
// → silent success (code 0, no line), and a context.Canceled — even wrapped by an
// engine error — → the interrupt line with the conventional 130, never a raw
// "context canceled".
func TestExitStatus_InterruptAndSuccess(t *testing.T) {
	root := newRootCmd()
	if msg, code := exitStatus(nil, root); msg != "" || code != 0 {
		t.Errorf("nil error → (%q, %d), want (\"\", 0)", msg, code)
	}
	wrapped := fmt.Errorf("sync aborted: %w", context.Canceled)
	for _, err := range []error{context.Canceled, wrapped} {
		msg, code := exitStatus(err, root)
		if code != 130 {
			t.Errorf("%v → exit %d, want 130", err, code)
		}
		if msg != "ksync: interrupted" {
			t.Errorf("%v → msg %q, want %q", err, msg, "ksync: interrupted")
		}
	}
}
