package ui

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// fixedClock makes timestamps deterministic. A bytes.Buffer is not an *os.File
// so NewColors disables color automatically — these tests assert on plain text.
func fixedClock() time.Time {
	return time.Date(2026, 6, 13, 15, 4, 5, 0, time.UTC)
}

// New must honor its documented os.Stderr default when Writer is unset —
// otherwise a pure-log run (no pipeline binds a writer) panics on the first
// record written to a nil io.Writer.
func TestNew_DefaultsWriterToStderr(t *testing.T) {
	s, ok := New(Options{}).GetSink().(*sink)
	if !ok {
		t.Fatalf("New did not return a *sink")
	}
	if s.w != os.Stderr {
		t.Errorf("New(Options{}).w = %v, want os.Stderr", s.w)
	}
}

func TestSink_InfoFormat(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Writer: &buf, Clock: fixedClock})
	log.Info("synced", "app", "ns-system", "objects", 26)

	got := buf.String()
	want := "15:04:05 • synced  app=ns-system objects=26\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestSink_ErrorFormat(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Writer: &buf, Clock: fixedClock})
	log.Error(errString("boom"), "sync failed", "app", "shop")

	got := buf.String()
	if !strings.HasPrefix(got, "15:04:05 ✗ sync failed") {
		t.Errorf("error line missing symbol/message: %q", got)
	}
	for _, want := range []string{"app=shop", `error=boom`} {
		if !strings.Contains(got, want) {
			t.Errorf("error line %q missing %q", got, want)
		}
	}
}

func TestSink_QuietDropsInfoKeepsError(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Writer: &buf, Clock: fixedClock, Quiet: true})
	log.Info("syncing", "engine", "internal") // must be dropped
	log.Error(errString("unauthorized"), "list failed")

	got := buf.String()
	if strings.Contains(got, "syncing") {
		t.Errorf("quiet sink leaked an Info line: %q", got)
	}
	if !strings.Contains(got, "list failed") {
		t.Errorf("quiet sink dropped an Error line: %q", got)
	}
}

// The quiet engine logger swallows gitops-engine's benign "Partial success"
// discovery notice (logged at Error level on a cold cluster while an aggregated
// APIService warms up) but still surfaces a real Error — and a non-quiet logger
// keeps even the benign notice, since only the engine/klog stream is filtered.
func TestSink_QuietDropsBenignDiscoveryNotice(t *testing.T) {
	var buf bytes.Buffer
	quiet := New(Options{Writer: &buf, Clock: fixedClock, Quiet: true})
	quiet.Error(errString("metrics.k8s.io/v1beta1: stale GroupVersion discovery"),
		"Partial success when performing preferred resource discovery")
	quiet.Error(errString("connection refused"), "real failure")

	got := buf.String()
	if strings.Contains(got, "Partial success") {
		t.Errorf("quiet sink leaked the benign discovery notice: %q", got)
	}
	if !strings.Contains(got, "real failure") {
		t.Errorf("quiet sink dropped a real Error: %q", got)
	}

	buf.Reset()
	loud := New(Options{Writer: &buf, Clock: fixedClock})
	loud.Error(nil, "Partial success when performing preferred resource discovery")
	if !strings.Contains(buf.String(), "Partial success") {
		t.Errorf("non-quiet sink should not filter: %q", buf.String())
	}
}

func TestSink_VerbosityGate(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Writer: &buf, Clock: fixedClock, Verbosity: 0})
	log.V(1).Info("change detected", "path", "x")
	if buf.Len() != 0 {
		t.Errorf("V(1) printed at verbosity 0: %q", buf.String())
	}
	log.V(0).Info("synced")
	if !strings.Contains(buf.String(), "synced") {
		t.Errorf("V(0) suppressed at verbosity 0: %q", buf.String())
	}
}

func TestSink_QuotesValuesWithSpaces(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Writer: &buf, Clock: fixedClock})
	log.Info("built", "command", "docker buildx bake")
	if !strings.Contains(buf.String(), `command="docker buildx bake"`) {
		t.Errorf("value with spaces was not quoted: %q", buf.String())
	}
}

func TestSink_WithValuesMerged(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Writer: &buf, Clock: fixedClock})
	log.WithValues("app", "shop").Info("synced", "objects", 3)
	got := buf.String()
	if !strings.Contains(got, "app=shop") || !strings.Contains(got, "objects=3") {
		t.Errorf("WithValues context not merged: %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
