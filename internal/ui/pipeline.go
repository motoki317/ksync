package ui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Stage icons distinguish the kinds of work at a glance; the kind word beside
// each names it explicitly so the emoji is never the only signal. Exported so the
// committed summary line (cmd) leads with the same 🚢 as the live Deploy stage.
const (
	IconBuild  = "🔨" // docker build / bake
	IconImport = "📦" // imageLoad into the cluster store
	IconDeploy = "🚢" // apply / ship to the cluster
)

// Stage phases order the rows within a pipeline regardless of when each stage is
// added: builds first, then their imports, then the single deploy — which is
// created up front (pending) so the developer sees it waiting while builds run.
const (
	phaseBuild = iota
	phaseImport
	phaseDeploy
)

type stageState int

const (
	statePending stageState = iota
	stateRunning
	stateDone
	stateFailed
)

// Stage is one row of a Pipeline: a single unit of work (one build batch, one
// import, the deploy) with a lifecycle pending → running → done/failed. Build
// and Import stages are born running (their work starts immediately); the Deploy
// stage is born pending and Start()ed when the apply begins, so it shows as
// upcoming work beneath the builds. A Stage is also the io.Writer a build or
// import command's combined output is wired to: it tails the latest line live
// and buffers the rest, surfacing the full log only if the command fails.
type Stage struct {
	pipe   *Pipeline
	icon   string
	kind   string
	phase  int
	label  string
	silent bool // off a terminal, prints no standalone result line (the deploy: its record is the committed summary)

	mu    sync.Mutex
	state stageState
	start time.Time
	end   time.Time
	tail  string
	buf   bytes.Buffer
}

// Start moves a pending stage to running (no-op once running). The Deploy stage
// uses it; Build/Import stages are already running when created.
func (s *Stage) Start() {
	s.mu.Lock()
	if s.state == statePending {
		s.state = stateRunning
		s.start = s.pipe.now()
	}
	s.mu.Unlock()
	s.pipe.refresh()
}

// SetTail updates the trailing detail shown while the stage runs (the deploy's
// "N not ready", say). The next tick repaints it.
func (s *Stage) SetTail(line string) {
	s.mu.Lock()
	s.tail = line
	s.mu.Unlock()
}

// Write captures a command's output and feeds its latest non-empty line to the
// live row. It never blocks the command and never errors.
func (s *Stage) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.buf.Write(p)
	s.mu.Unlock()
	if line := lastNonEmptyLine(p); line != "" {
		s.SetTail(sanitizeLine(line))
	}
	return len(p), nil
}

// Done finishes the stage as succeeded or failed and stamps its elapsed time.
// On failure it prints the full captured output above the live block so the
// developer can see what went wrong; the row itself stays (as ✗) until the
// whole pipeline is committed. Off a terminal it prints a single plain result
// line instead, keeping piped output clean.
func (s *Stage) Done(err error) {
	s.mu.Lock()
	s.end = s.pipe.now()
	if s.state == statePending {
		s.start = s.end // never started explicitly (e.g. an empty deploy)
	}
	if err != nil {
		s.state = stateFailed
	} else {
		s.state = stateDone
	}
	elapsed := s.end.Sub(s.start)
	out := strings.TrimRight(s.buf.String(), "\n")
	s.mu.Unlock()

	// On failure, surface the full captured output above the block in both modes
	// so the developer (or a CI log) sees what broke, not just the ✗.
	if err != nil && out != "" {
		liveTerm.line(s.pipe.w, fmt.Sprintf("%s\n%s\n", s.pipe.c.Dim("─── "+s.heading()+" output ───"), out))
	}
	if !s.pipe.tty {
		// The deploy is silent on a pipe (its committed summary already reports the
		// app); build/import lines are not — they are the only record of that work.
		if !s.silent {
			liveTerm.line(s.pipe.w, s.plainResult(err, elapsed))
		}
		return
	}
	s.pipe.refresh()
}

// heading names the stage for the failure-log header and the non-terminal result
// line: "Build ns-dashboard", "Deploy ns-system" (the app when the stage has no
// label of its own, as the deploy does not).
func (s *Stage) heading() string {
	label := s.label
	if label == "" {
		label = s.pipe.app
	}
	return s.kind + " " + label
}

func (s *Stage) plainResult(err error, d time.Duration) string {
	mark := s.pipe.c.Green("✓")
	if err != nil {
		mark = s.pipe.c.Red("✗")
	}
	return fmt.Sprintf("%s %s %s  %s\n", mark, s.icon, s.heading(), Elapsed(s.pipe.c, d))
}

// Pipeline is one app's live progress group: a header line (app name) with its
// build, import, and deploy stages as indented rows beneath it. Several
// pipelines animate at once (apps sync concurrently); liveTerm renders them as
// one block. A pipeline with only a deploy stage (an app with no builds) and no
// forced expansion collapses to a single line, so infra apps stay compact while
// build apps show the full tree.
type Pipeline struct {
	w      io.Writer
	c      Colors
	app    string
	expand bool // app has builds: always show the tree, never collapse
	tty    bool
	now    func() time.Time

	mu     sync.Mutex
	stages []*Stage
	deploy *Stage // cached so Deploy() is idempotent
}

// StartPipeline begins one app's progress group. On an interactive terminal it
// adds an animated group to the live block; otherwise it stays inert (stages
// print plain result lines on Done), so piped/redirected output stays sane.
// expand forces the multi-row tree for an app that has builds, so its deploy
// stage shows as a pending row from the start rather than collapsing.
func StartPipeline(w io.Writer, c Colors, app string, expand bool) *Pipeline {
	return startPipeline(w, c, app, expand, time.Now)
}

func startPipeline(w io.Writer, c Colors, app string, expand bool, now func() time.Time) *Pipeline {
	p := &Pipeline{w: w, c: c, app: app, expand: expand, now: now}
	f, ok := w.(*os.File)
	p.tty = ok && c.Enabled() && term.IsTerminal(int(f.Fd()))
	if p.tty {
		fd := int(f.Fd())
		liveTerm.addItem(w, func() int { return cols(fd) }, func() int { return rows(fd) }, p)
	}
	return p
}

func (p *Pipeline) add(s *Stage) *Stage {
	p.mu.Lock()
	p.stages = append(p.stages, s)
	p.mu.Unlock()
	p.refresh()
	return s
}

// Build adds a running build stage for one build batch (label = the batch name).
func (p *Pipeline) Build(label string) *Stage {
	return p.add(&Stage{pipe: p, icon: IconBuild, kind: "Build", phase: phaseBuild, label: label, state: stateRunning, start: p.now()})
}

// Import adds a running import stage for one build batch made visible to the
// cluster (label = the batch name).
func (p *Pipeline) Import(label string) *Stage {
	return p.add(&Stage{pipe: p, icon: IconImport, kind: "Import", phase: phaseImport, label: label, state: stateRunning, start: p.now()})
}

// Deploy returns the app's single deploy stage, created pending on first call so
// it shows as upcoming work while builds run; later calls return the same stage.
func (p *Pipeline) Deploy() *Stage {
	p.mu.Lock()
	if p.deploy == nil {
		p.deploy = &Stage{pipe: p, icon: IconDeploy, kind: "Deploy", phase: phaseDeploy, state: statePending, silent: true}
		p.stages = append(p.stages, p.deploy)
	}
	d := p.deploy
	p.mu.Unlock()
	p.refresh()
	return d
}

// Finish commits the group: it prints summary (the committed app line, with any
// failure/degraded rows above it) to scrollback in place of the live group and
// removes the group from the block. Off a terminal it just prints summary.
func (p *Pipeline) Finish(summary string) {
	if p.tty {
		liveTerm.finishItem(p, summary)
		return
	}
	liveTerm.line(p.w, summary)
}

func (p *Pipeline) refresh() {
	if p.tty {
		liveTerm.refresh()
	}
}

// lines renders the group for the given spinner frame: a single collapsed line
// for a deploy-only app, else an app header with its stages sorted by phase as
// indented rows. Called from the ticker goroutine, so it locks the pipeline and
// reads each stage under its own lock.
func (p *Pipeline) lines(frame rune) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	stages := make([]*Stage, len(p.stages))
	copy(stages, p.stages)
	sort.SliceStable(stages, func(i, j int) bool { return stages[i].phase < stages[j].phase })

	if !p.expand && len(stages) == 1 {
		return []string{p.collapsedLine(stages[0], frame)}
	}

	// Align the meta column: pad every label to the widest in the group.
	labelW := 0
	for _, s := range stages {
		if w := displayWidth(s.label); w > labelW {
			labelW = w
		}
	}
	out := make([]string, 0, len(stages)+1)
	out = append(out, fmt.Sprintf("%s %s", p.c.Cyan(string(frame)), p.c.Bold(p.app)))
	for _, s := range stages {
		out = append(out, p.stageRow(s, frame, labelW))
	}
	return out
}

// stageRow renders one indented stage row, columns aligned: status symbol, icon,
// kind (padded), label (padded), then the running tail and elapsed.
func (p *Pipeline) stageRow(s *Stage, frame rune, labelW int) string {
	sym, meta := p.stageParts(s, frame)
	label := s.label + strings.Repeat(" ", labelW-displayWidth(s.label))
	row := fmt.Sprintf("    %s %s %-6s  %s", sym, s.icon, s.kind, label)
	if meta != "" {
		row += "  " + meta
	}
	return strings.TrimRight(row, " ")
}

// collapsedLine renders a single-stage pipeline (deploy-only app) as one line:
// status symbol, deploy icon, kind, app name, then the running tail and elapsed.
func (p *Pipeline) collapsedLine(s *Stage, frame rune) string {
	sym, meta := p.stageParts(s, frame)
	line := fmt.Sprintf("%s %s %s %s", sym, s.icon, s.kind, p.c.Bold(p.app))
	if meta != "" {
		line += "  " + meta
	}
	return line
}

// stageParts returns a stage's status symbol and its meta (the dim tail plus the
// elapsed time) for the current frame, reading the stage under its own lock.
func (p *Pipeline) stageParts(s *Stage, frame rune) (sym, meta string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case statePending:
		return p.c.Dim("○"), ""
	case stateRunning:
		meta = Elapsed(p.c, p.now().Sub(s.start))
		if s.tail != "" {
			meta = p.c.Dim(s.tail) + "  " + meta
		}
		return p.c.Cyan(string(frame)), meta
	case stateFailed:
		return p.c.Red("✗"), Elapsed(p.c, s.end.Sub(s.start))
	default: // stateDone
		return p.c.Green("✓"), Elapsed(p.c, s.end.Sub(s.start))
	}
}
