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

// CommitInfo carries the deploy outcome the committed view needs but the
// pipeline does not itself track: the dim apply summary ("21 applied, 2 pruned"),
// the health symbol for the deploy row/line (✓ healthy · ⚠ degraded · ✗ failed),
// any per-resource failure/degraded lines to print above the block, and the
// app-name column width that aligns the deploy-only single line across a run.
type CommitInfo struct {
	Summary string
	Symbol  string
	Above   []string
	NameW   int
}

// Finish commits the group in place of the live one and removes it from the
// block. A build app keeps its full stage tree frozen — every build/import row
// and the deploy row retain their final time — so the per-stage timings survive
// the run; a build-less app collapses to one deploy line. Off a terminal it
// prints only that single line (the build/import stages already streamed their
// own result lines as they finished).
func (p *Pipeline) Finish(info CommitInfo) {
	block := p.committed(info)
	if p.tty {
		liveTerm.finishItem(p, block)
		return
	}
	liveTerm.line(p.w, block)
}

// Discard removes the live group without committing a summary — used when a run
// failed before producing one (its error is reported elsewhere). A no-op off a
// terminal, where there is no pinned group to remove.
func (p *Pipeline) Discard() {
	if p.tty {
		liveTerm.finishItem(p, "")
	}
}

// committed renders the frozen committed block: the per-resource failure/degraded
// lines first, then a build app's full stage tree (so each stage's final time is
// preserved) or a build-less app's single deploy line. Off a terminal it is
// always the single line — the build/import rows already printed as they ran.
func (p *Pipeline) committed(info CommitInfo) string {
	lines := append([]string{}, info.Above...)
	if p.tty && p.expand {
		lines = append(lines, p.committedTree(info)...)
	} else {
		lines = append(lines, p.committedLine(info))
	}
	return strings.Join(lines, "\n") + "\n"
}

// committedTree freezes a build app's group: the app header, then each stage as a
// row carrying its final symbol and elapsed (the deploy row also the apply
// summary), ordered build→import→deploy and column-aligned like the live tree.
func (p *Pipeline) committedTree(info CommitInfo) []string {
	p.mu.Lock()
	stages := make([]*Stage, len(p.stages))
	copy(stages, p.stages)
	p.mu.Unlock()
	sort.SliceStable(stages, func(i, j int) bool { return stages[i].phase < stages[j].phase })

	labelW := 0
	for _, s := range stages {
		if w := displayWidth(committedLabel(s)); w > labelW {
			labelW = w
		}
	}
	out := make([]string, 0, len(stages)+1)
	out = append(out, p.c.Bold(p.app))
	for _, s := range stages {
		out = append(out, p.committedRow(s, info, labelW))
	}
	return out
}

// committedRow renders one frozen stage row: status symbol, icon, padded label,
// then its elapsed. The deploy row additionally takes the health symbol and apply
// summary from info — the pipeline only knows the row's time, not its outcome.
func (p *Pipeline) committedRow(s *Stage, info CommitInfo, labelW int) string {
	s.mu.Lock()
	failed := s.state == stateFailed
	elapsed := s.end.Sub(s.start)
	s.mu.Unlock()

	sym := p.c.Green("✓")
	if failed {
		sym = p.c.Red("✗")
	}
	meta := Elapsed(p.c, elapsed)
	if s.phase == phaseDeploy {
		if info.Symbol != "" {
			sym = info.Symbol
		}
		if info.Summary != "" {
			meta = info.Summary + "  " + meta
		}
	}
	label := committedLabel(s)
	label += strings.Repeat(" ", labelW-displayWidth(label))
	return strings.TrimRight(fmt.Sprintf("    %s %s %s  %s", sym, s.icon, label, meta), " ")
}

// committedLabel is a stage's name in the committed tree: its build/import label,
// or "deploy" for the deploy row — whose own label is empty (the app heads the
// group), so the row needs a word of its own.
func committedLabel(s *Stage) string {
	if s.phase == phaseDeploy {
		return "deploy"
	}
	return s.label
}

// committedLine renders a build-less app's single committed line, reading the
// deploy stage's own elapsed (the apply + health-gate time) so the number is that
// stage's, not the app's end-to-end wall clock.
func (p *Pipeline) committedLine(info CommitInfo) string {
	var elapsed time.Duration
	if p.deploy != nil {
		p.deploy.mu.Lock()
		if p.deploy.end.After(p.deploy.start) {
			elapsed = p.deploy.end.Sub(p.deploy.start)
		}
		p.deploy.mu.Unlock()
	}
	return DeployLine(p.c, p.app, info.Summary, info.Symbol, elapsed, info.NameW)
}

// DeployLine renders one app's committed deploy line — the single-row form used
// for a build-less app and by `ksync destroy`: a health symbol, the 🚢 icon, the
// app name (padded to nameW so a run's lines align), the dim apply summary, and
// the elapsed time (omitted when zero). It carries no "Deploy" kind word: the
// icon and the "N applied" summary already say what happened and the app is the
// subject, so the committed line never collides with the in-pipeline Deploy stage.
func DeployLine(c Colors, app, summary, symbol string, elapsed time.Duration, nameW int) string {
	if symbol == "" {
		symbol = c.Green("✓")
	}
	name := c.Bold(app)
	if pad := nameW - displayWidth(app); pad > 0 {
		name += strings.Repeat(" ", pad)
	}
	line := fmt.Sprintf("%s %s %s", symbol, IconDeploy, name)
	if summary != "" {
		line += "  " + summary
	}
	if elapsed > 0 {
		line += "  " + Elapsed(c, elapsed)
	}
	return line
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
