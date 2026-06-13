package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// fixedClock makes timestamps deterministic. A bytes.Buffer is not an *os.File
// so NewColors disables color automatically — these tests assert on plain text.
func fixedClock() time.Time {
	return time.Date(2026, 6, 13, 15, 4, 5, 0, time.UTC)
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
