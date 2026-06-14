package ui

import (
	"bytes"
	"testing"
)

// On a non-terminal writer the wait line is inert: it animates nothing and emits
// nothing, so redirected or piped sync output is never polluted by progress
// frames. Status and Stop must be safe no-ops in that state.
func TestWaiting_InertOffTerminal(t *testing.T) {
	var buf bytes.Buffer
	w := StartWaiting(&buf, NewColors(&buf), "🚢 api-b  waiting for health")
	w.Status("2 not ready")
	w.Stop()
	w.Stop() // idempotent
	if buf.Len() != 0 {
		t.Errorf("inert Waiting wrote %q, want nothing", buf.String())
	}
}
