package ui

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Options configure a Sink.
type Options struct {
	// Writer receives the formatted lines; defaults to os.Stderr via New.
	Writer io.Writer
	// Verbosity is the highest V(level) that is shown. 0 keeps the loop's
	// status messages; 1+ surfaces the per-change tracing.
	Verbosity int
	// Quiet drops all Info/V output and keeps only Error. This is how the
	// engine and the routed client-go (klog) stream are silenced down to
	// genuine failures while ksync's own status messages still print.
	Quiet bool
	// Clock is the time source for line timestamps; defaults to time.Now.
	// Injectable so tests get deterministic output.
	Clock func() time.Time
}

// New returns a logr.Logger that renders clean, colored, single-line records.
// It satisfies the same logr.Logger contract the engine and loop already take,
// so it drops into the existing plumbing without rewiring.
func New(opts Options) logr.Logger {
	w := opts.Writer
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return logr.New(&Sink{
		mu:        &sync.Mutex{},
		w:         w,
		colors:    NewColors(w),
		verbosity: opts.Verbosity,
		quiet:     opts.Quiet,
		clock:     clock,
	})
}

// Sink is the logr.LogSink behind New. It formats records as
//
//	HH:MM:SS <symbol> <message>   key=value key=value
//
// with the timestamp and key/value tail dimmed so the eye lands on the
// message. WithName/WithValues are honored so engine sub-loggers keep their
// context.
type Sink struct {
	mu        *sync.Mutex // pointer: shared across WithName/WithValues copies so writes stay line-atomic
	w         io.Writer
	colors    Colors
	verbosity int
	quiet     bool
	clock     func() time.Time
	name      string
	values    []any
}

func (s *Sink) Init(logr.RuntimeInfo) {}

// Enabled gates Info/V output. Returning false in quiet mode means the engine
// never even formats its chatter (and klog skips its own V-checks), while
// Error bypasses Enabled entirely in logr — so failures still surface.
func (s *Sink) Enabled(level int) bool {
	if s.quiet {
		return false
	}
	return level <= s.verbosity
}

func (s *Sink) Info(_ int, msg string, kv ...any) {
	s.write(s.colors.Cyan("•"), msg, kv, false)
}

func (s *Sink) Error(err error, msg string, kv ...any) {
	if err != nil {
		kv = append(kv, "error", err.Error())
	}
	s.write(s.colors.Red("✗"), msg, kv, true)
}

func (s *Sink) write(symbol, msg string, kv []any, isError bool) {
	var b strings.Builder
	b.WriteString(s.colors.Dim(s.clock().Format("15:04:05")))
	b.WriteByte(' ')
	b.WriteString(symbol)
	b.WriteByte(' ')
	if isError {
		b.WriteString(s.colors.Red(msg))
	} else {
		b.WriteString(s.colors.Bold(msg))
	}
	if tail := s.formatPairs(kv); tail != "" {
		b.WriteString("  ")
		b.WriteString(s.colors.Dim(tail))
	}
	b.WriteByte('\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = io.WriteString(s.w, b.String())
}

// formatPairs renders the merged WithName/WithValues context and the call's
// own key/value pairs as a stable, space-separated "key=value" tail. Keys are
// sorted so the same record always reads the same way; values with spaces are
// quoted so the boundaries stay obvious.
func (s *Sink) formatPairs(kv []any) string {
	pairs := map[string]string{}
	var order []string
	add := func(list []any) {
		for i := 0; i+1 < len(list); i += 2 {
			key := fmt.Sprint(list[i])
			if _, seen := pairs[key]; !seen {
				order = append(order, key)
			}
			pairs[key] = quoteValue(list[i+1])
		}
	}
	add(s.values)
	add(kv)
	if s.name != "" {
		if _, seen := pairs["logger"]; !seen {
			order = append(order, "logger")
		}
		pairs["logger"] = s.name
	}
	sort.Strings(order)

	var parts []string
	for _, k := range order {
		parts = append(parts, k+"="+pairs[k])
	}
	return strings.Join(parts, " ")
}

func quoteValue(v any) string {
	s := fmt.Sprint(v)
	if s == "" || strings.ContainsAny(s, " \t\"") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func (s *Sink) WithValues(kv ...any) logr.LogSink {
	c := *s
	c.values = append(append([]any{}, s.values...), kv...)
	return &c
}

func (s *Sink) WithName(name string) logr.LogSink {
	c := *s
	if c.name != "" {
		c.name += "." + name
	} else {
		c.name = name
	}
	return &c
}
