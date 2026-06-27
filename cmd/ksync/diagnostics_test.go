package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/motoki317/ksync/internal/engine"
)

// shouldDiagnose dumps on a health-gate timeout regardless of how the run ended,
// and on a cancellation only when the user actually interrupted — a sibling app's
// failure cancels this app too, but that aborted-but-progressing app must stay
// quiet (the failed sibling reports its own error).
func TestShouldDiagnose(t *testing.T) {
	timeout := &engine.TimeoutError{App: "api"}
	wrappedTimeout := fmt.Errorf("app api: %w", timeout)
	wrappedCancel := fmt.Errorf("app api: %w", context.Canceled)

	cases := []struct {
		name            string
		err             error
		userInterrupted bool
		want            bool
	}{
		{"timeout, no interrupt", timeout, false, true},
		{"timeout wrapped, no interrupt", wrappedTimeout, false, true},
		{"timeout always dumps even mid-interrupt", timeout, true, true},
		{"user Ctrl-C of a stuck app", context.Canceled, true, true},
		{"user Ctrl-C, wrapped cancel", wrappedCancel, true, true},
		{"sibling-abort cancels this app, not a user interrupt", context.Canceled, false, false},
		{"plain apply error reports itself, no dump", errors.New("admission webhook denied"), true, false},
		{"deadline exceeded is not a bare cancel path", context.DeadlineExceeded, true, false},
	}
	for _, tc := range cases {
		if got := shouldDiagnose(tc.err, tc.userInterrupted); got != tc.want {
			t.Errorf("%s: shouldDiagnose(%v, interrupted=%v) = %v, want %v", tc.name, tc.err, tc.userInterrupted, got, tc.want)
		}
	}
}
