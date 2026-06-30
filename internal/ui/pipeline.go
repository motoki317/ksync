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
// import command's combined output is wired to. On a terminal it tails the latest
// line on the live row and buffers the rest, surfacing the full log only if the
// command fails. Off a terminal it streams each complete build line live, prefixed
// with the app/stage it belongs to (the buildx model — the log itself is the
// liveness signal); import output is buffered there too (a coalesced load tees one
// command to every waiting app, so it is shown only on failure, never per app).
type Stage struct {
	pipe  *Pipeline
	icon  string
	kind  string
	phase int
	label string

	mu      sync.Mutex
	state   stageState
	start   time.Time
	end     time.Time
	tail    string
	buf     bytes.Buffer // full output, for the on-terminal failure dump (and off-terminal import failures)
	partial []byte       // off-terminal: bytes since the last newline, awaiting a complete line to stream
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
// "N not ready", say). On a terminal the next tick repaints it on the live row.
// Off a terminal there is no row to repaint, so a *changed* tail is streamed as
// one prefixed line — the deploy's health gate calls this each poll, and an
// unchanged status prints nothing, so only real progress (the not-ready set
// shrinking) emits, never a timed reprint of the same elapsed.
func (s *Stage) SetTail(line string) {
	s.mu.Lock()
	changed := line != s.tail
	s.tail = line
	s.mu.Unlock()
	if changed && line != "" && !s.pipe.tty {
		liveTerm.line(SectionPipeline, s.pipe.w, s.streamPrefix()+line+"\n")
	}
}

// getState reads the stage's lifecycle state under its lock — used by the
// pipeline's collapse decision, which must tell a started deploy from a pending one.
func (s *Stage) getState() stageState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Write captures a command's output. On a terminal it buffers the whole output
// (for the failure dump) and feeds its latest non-empty line to the live row. Off
// a terminal it streams instead — see writeOff. It never blocks the command and
// never errors.
func (s *Stage) Write(p []byte) (int, error) {
	if !s.pipe.tty {
		return s.writeOff(p)
	}
	s.mu.Lock()
	s.buf.Write(p)
	s.mu.Unlock()
	if line := lastNonEmptyLine(p); line != "" {
		s.SetTail(sanitizeLine(line))
	}
	return len(p), nil
}

// writeOff streams a command's output off a terminal, the buildx model: each
// complete build line is printed live, prefixed with the app/stage it belongs to,
// so a long build's progress shows in a pipe or CI log without any synthetic
// heartbeat. Import stages do not stream — build.Loader coalesces a load and tees
// one command's output to every waiting app's stage, so streaming per stage would
// print each line once per app; their output is buffered and surfaced only if the
// load fails. Build stages keep no full buffer (their lines already streamed, so a
// failure needs no re-dump).
func (s *Stage) writeOff(p []byte) (int, error) {
	if s.phase == phaseImport {
		s.mu.Lock()
		s.buf.Write(p)
		s.mu.Unlock()
		return len(p), nil
	}
	s.mu.Lock()
	s.partial = append(s.partial, p...)
	lines := s.takeLines()
	s.mu.Unlock()
	for _, line := range lines {
		s.emitLine(line)
	}
	return len(p), nil
}

// takeLines splits the complete (newline-terminated) lines out of the partial
// buffer, leaving any trailing partial line for the next write. Caller holds s.mu.
func (s *Stage) takeLines() []string {
	var lines []string
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string(s.partial[:i]))
		s.partial = s.partial[i+1:]
	}
	if len(s.partial) == 0 {
		s.partial = nil // drop the reslice's backing array so it cannot grow unbounded
	}
	return lines
}

// emitLine prints one streamed output line with its stage prefix, skipping a line
// blank after cleaning so a build tool's empty line never becomes a bare prefix.
// Called without s.mu held — it writes through the console.
func (s *Stage) emitLine(line string) {
	if line = streamClean(line); line == "" {
		return
	}
	liveTerm.line(SectionPipeline, s.pipe.w, s.streamPrefix()+line+"\n")
}

// streamPrefix tags a streamed line with the app, the build/import label, and the
// phase word — "shop/ui build │ ", "shop/ui import │ ", "db deploy │ " — so the
// interleaved lines of concurrent apps stay attributable and greppable by phase
// (the word disambiguates a build from an import that share a label). A label equal
// to the app name (a single unnamed build whose image's last segment is the app) is
// dropped, so "web/web build" reads as "web build".
func (s *Stage) streamPrefix() string {
	name := s.pipe.app
	if s.label != "" && s.label != s.pipe.app {
		name += "/" + s.label
	}
	return name + " " + strings.ToLower(s.kind) + " " + s.pipe.c.Dim("│") + " "
}

// Done finishes the stage as succeeded or failed and stamps its elapsed time.
// On a terminal a failure prints the full captured output above the live block so
// the developer can see what went wrong; the row itself stays (as ✗) until the
// whole pipeline is committed. Off a terminal it streams a result line instead —
// see doneOff.
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
	partial := string(s.partial)
	s.partial = nil
	s.mu.Unlock()

	if !s.pipe.tty {
		s.doneOff(err, elapsed, out, partial)
		return
	}
	// On a terminal, surface the full captured output above the block on failure so
	// the developer sees what broke, not just the ✗.
	if err != nil && out != "" {
		liveTerm.line(SectionPipeline, s.pipe.w, fmt.Sprintf("%s\n%s\n", s.pipe.c.Dim("─── "+s.heading()+" output ───"), out))
	}
	s.pipe.refresh()
}

// doneOff finishes a stage off a terminal. The deploy stage prints nothing here —
// its result is the committed deploy line (Pipeline.Finish). A build/import stage
// flushes any trailing partial line, then prints one prefixed result line
// ("shop/ui build │ ✓ 1m01s"). A failure prints the captured output first when
// there is any — an import never streamed its output, so its buffer is dumped;
// a build already streamed every line, so its buffer is empty and only the ✗
// result prints.
func (s *Stage) doneOff(err error, elapsed time.Duration, out, partial string) {
	if s.phase == phaseDeploy {
		return
	}
	s.emitLine(partial)
	if err != nil && out != "" {
		liveTerm.line(SectionPipeline, s.pipe.w, fmt.Sprintf("%s\n%s\n", s.pipe.c.Dim("─── "+s.heading()+" output ───"), out))
	}
	mark := "✓"
	if err != nil {
		mark = "✗"
	}
	liveTerm.line(SectionPipeline, s.pipe.w, s.streamPrefix()+mark+" "+Elapsed(s.pipe.c, elapsed)+"\n")
}

// heading names the stage for the failure-log header and the non-terminal result
// line, always carrying the owning app so two apps that build like-named images
// are never conflated: "Build shop/ui" (app-qualified label), "Deploy ns-system"
// (the deploy has no label of its own, so the app stands alone).
func (s *Stage) heading() string {
	if s.label == "" {
		return s.kind + " " + s.pipe.app
	}
	return s.kind + " " + s.pipe.app + "/" + s.label
}

// Pipeline is one app's live progress group: a header line (app name plus the
// group's total wall-clock) with its build, import, and deploy stages as indented
// rows beneath it. Several pipelines animate at once (apps sync concurrently);
// liveTerm renders them as one block. A pipeline that has only a deploy stage
// collapses to a single line — an app with no builds, or a build app whose images
// were all overridden so it never built — so infra and override-only apps stay
// compact while apps that actually build show the full tree.
type Pipeline struct {
	w      io.Writer
	c      Colors
	app    string
	expand bool // app has builds: hold the tree open (pending deploy row) until builds begin
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
	p.tty = isLive(w, c)
	if p.tty {
		fd := int(w.(*os.File).Fd())
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
		p.deploy = &Stage{pipe: p, icon: IconDeploy, kind: "Deploy", phase: phaseDeploy, state: statePending}
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
// block. On a terminal an app that built keeps its full stage tree frozen — every
// build/import row and the deploy row retain their final time, the header its
// group total — while a deploy-only app collapses to one deploy line. Off a
// terminal every app commits its single deploy line: a build app's per-stage
// results already streamed as they ran, so only the deploy result remains, and the
// slowest-first recap (Recap) carries the per-app totals.
func (p *Pipeline) Finish(info CommitInfo) {
	block := p.committed(info)
	if p.tty {
		liveTerm.finishItem(p, block)
		return
	}
	liveTerm.line(SectionPipeline, p.w, block)
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
// lines first, then the app's summary. A build app on a terminal keeps its full
// stage tree (each stage's final time preserved); off a terminal it commits just
// its deploy line, since the build/import stages already streamed their result
// lines as they ran (ADR 20260701-non-terminal-log-streaming). A deploy-only app
// is its single deploy line in either mode.
func (p *Pipeline) committed(info CommitInfo) string {
	lines := append([]string{}, info.Above...)
	if p.hasBuildStages() && p.tty {
		lines = append(lines, p.committedTree(info)...)
	} else {
		lines = append(lines, p.committedLine(info))
	}
	return strings.Join(lines, "\n") + "\n"
}

// oneLine renders a build app's whole pipeline as a single non-terminal line: the
// group health symbol, the app, each finished stage as "icon label time" (the
// deploy carries its apply summary instead of a label, which the app already
// supplies) joined by " · ", then the group's total wall-clock. It is a build app's
// entry in the slowest-first recap (Recap) printed after the run's Summary — the
// consolidated "which app took how long" view; the per-stage progress itself
// streamed live as the build ran. Pending stages (a deploy that never ran because a
// build failed) are skipped.
func (p *Pipeline) oneLine(info CommitInfo) string {
	p.mu.Lock()
	stages := make([]*Stage, len(p.stages))
	copy(stages, p.stages)
	p.mu.Unlock()
	sort.SliceStable(stages, func(i, j int) bool { return stages[i].phase < stages[j].phase })

	sym := info.Symbol
	if sym == "" {
		sym = p.c.Green("✓")
	}
	var segs []string
	for _, s := range stages {
		s.mu.Lock()
		pending := s.state == statePending
		elapsed := s.end.Sub(s.start)
		s.mu.Unlock()
		if pending {
			continue
		}
		seg := s.icon
		if s.phase == phaseDeploy {
			if info.Summary != "" {
				seg += " " + info.Summary
			}
		} else if s.label != "" {
			seg += " " + s.label
		}
		seg += " " + Elapsed(p.c, elapsed)
		segs = append(segs, seg)
	}
	line := sym + " " + p.c.Bold(p.app)
	if len(segs) > 0 {
		line += "  " + strings.Join(segs, p.c.Dim(" · "))
	}
	// The total is the wall-clock span (builds overlap), worth showing only when
	// more than one stage ran — for a single stage it would just repeat its time.
	if len(segs) > 1 {
		if d := groupSpan(stages, p.now()); d > 0 {
			line += "  " + Elapsed(p.c, d)
		}
	}
	return line
}

// committedTree freezes a build app's group: the app header (with the group's
// total wall-clock), then each stage as a row carrying its final symbol and
// elapsed (the deploy row also the apply summary), ordered build→import→deploy and
// column-aligned like the live tree.
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
	header := p.c.Bold(p.app)
	if d := groupSpan(stages, p.now()); d > 0 {
		header += "  " + Elapsed(p.c, d)
	}
	out = append(out, header)
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

// committedLine renders a deploy-only app's single committed line — a build-less
// app, or a build app whose images were all overridden so it never built. It reads
// the deploy stage's own elapsed (the apply + health-gate time) so the number is
// that stage's, not the app's end-to-end wall clock, and leads with the "Deploy"
// kind word so it reads consistently with the deploy row of a build app's tree.
func (p *Pipeline) committedLine(info CommitInfo) string {
	var elapsed time.Duration
	if p.deploy != nil {
		p.deploy.mu.Lock()
		if p.deploy.end.After(p.deploy.start) {
			elapsed = p.deploy.end.Sub(p.deploy.start)
		}
		p.deploy.mu.Unlock()
	}
	return DeployLine(p.c, p.app, "Deploy", info.Summary, info.Symbol, elapsed, info.NameW)
}

// DeployLine renders one app's committed deploy line — the single-row form used
// for a deploy-only app and by `ksync destroy`: a health symbol, the 🚢 icon, an
// optional kind word ("Deploy"), the app name (padded to nameW so a run's lines
// align), the dim apply summary, and the elapsed time (omitted when zero). kind is
// the title shown beside the icon; the sync path passes "Deploy" so a deploy-only
// line matches the in-tree deploy row, while `ksync destroy` passes "" (it is not
// a deploy — its icon and summary already say what happened).
func DeployLine(c Colors, app, kind, summary, symbol string, elapsed time.Duration, nameW int) string {
	if symbol == "" {
		symbol = c.Green("✓")
	}
	line := fmt.Sprintf("%s %s", symbol, IconDeploy)
	if kind != "" {
		line += " " + kind
	}
	name := c.Bold(app)
	if pad := nameW - displayWidth(app); pad > 0 {
		name += strings.Repeat(" ", pad)
	}
	line += " " + name
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

// groupSpan is the whole group's wall-clock duration: from the earliest stage
// start to the latest stage end, or to now while any stage still runs. It is the
// span, not the sum of the rows — builds overlap, so "app took N" is the elapsed
// wall clock across the tree, not the addition of its stage times.
// Returns 0 when every stage is still pending (nothing has started to time).
// Each stage is read under its own lock; the caller passes the stage snapshot.
func groupSpan(stages []*Stage, now time.Time) time.Duration {
	var start, end time.Time
	running := false
	for _, s := range stages {
		s.mu.Lock()
		st, en, state := s.start, s.end, s.state
		s.mu.Unlock()
		if state == statePending {
			continue
		}
		if start.IsZero() || st.Before(start) {
			start = st
		}
		if state == stateRunning {
			running = true
		} else if en.After(end) {
			end = en
		}
	}
	if start.IsZero() {
		return 0
	}
	if running || end.Before(start) {
		end = now
	}
	return end.Sub(start)
}

// hasBuildStages reports whether any build or import row exists — i.e. the app
// actually built this run. A build app whose images were all supplied as overrides
// runs no build, so it has only the deploy stage and commits as the single deploy
// line rather than a one-child tree.
func (p *Pipeline) hasBuildStages() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.stages {
		if s.phase != phaseDeploy {
			return true
		}
	}
	return false
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

	// Collapse to one line when the deploy is the only stage: always for a
	// build-less app, and for a build app once its deploy has started while still
	// alone (its builds were all overridden, so no build/import rows will appear).
	// While the deploy is still pending, a build app keeps the pending deploy row
	// under its header so the upcoming work stays visible until builds begin.
	if len(stages) == 1 && stages[0].phase == phaseDeploy {
		if !p.expand || stages[0].getState() != statePending {
			return []string{p.collapsedLine(stages[0], frame)}
		}
	}

	// Align the meta column: pad every label to the widest in the group.
	labelW := 0
	for _, s := range stages {
		if w := displayWidth(s.label); w > labelW {
			labelW = w
		}
	}
	out := make([]string, 0, len(stages)+1)
	header := fmt.Sprintf("%s %s", p.c.Cyan(string(frame)), p.c.Bold(p.app))
	if d := groupSpan(stages, p.now()); d > 0 {
		header += "  " + Elapsed(p.c, d)
	}
	out = append(out, header)
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

// Recap returns this app's one-line completion summary and its total wall-clock,
// for the slowest-first timing recap printed after a run's Summary. ok is false on
// a terminal — there the frozen stage trees already show per-app timings in
// scrollback, so no recap is printed. Call after Finish, when every stage is
// frozen.
func (p *Pipeline) Recap(info CommitInfo) (line string, total time.Duration, ok bool) {
	if p.tty {
		return "", 0, false
	}
	p.mu.Lock()
	stages := make([]*Stage, len(p.stages))
	copy(stages, p.stages)
	p.mu.Unlock()
	if p.hasBuildStages() {
		return p.oneLine(info), groupSpan(stages, p.now()), true
	}
	return p.committedLine(info), groupSpan(stages, p.now()), true
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
