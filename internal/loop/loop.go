// Package loop is the watch-mode heart of ksync: it wires the file watcher,
// the dirty-set mapping, the scheduler, the builder, and the renderer into
// one event loop that builds, renders, and syncs apps as their inputs change.
// The cluster and docker sides are injected as SyncFunc/BuildFunc so the loop
// is testable without either.
package loop

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/build"
	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
	"github.com/motoki317/ksync/internal/schedule"
	"github.com/motoki317/ksync/internal/ui"
	"github.com/motoki317/ksync/internal/watch"
)

// SyncStats summarizes one app's apply for the loop's status line: how many
// objects the sync changed, pruned, or failed on, and how many of its live
// resources are Degraded afterwards. Applied counts only objects that actually
// differed, so a no-op re-sync reports 0 — the developer sees at a glance
// whether their edit changed anything. Degraded is a post-sync health snapshot
// (genuinely broken, not a rollout in flight); a healthy edit reports 0.
type SyncStats struct{ Applied, Pruned, Failed, Degraded int }

// SyncFunc applies one app's rendered objects to the cluster and reports how
// many changed, were pruned, or failed.
type SyncFunc func(ctx context.Context, app string, objects []*unstructured.Unstructured) (SyncStats, error)

// BuildFunc produces the images of one build batch and returns their full
// content-addressed refs in the same order. A batch is either a single
// ungrouped entry or all the dirty members of one build group (built by one
// bulk command); the loop forms the batches via App.BuildBatches. app is the
// owning app's name — a group can be built per-app (each app owns a subset of
// its images), so it is what distinguishes the two apps' progress lines.
type BuildFunc func(ctx context.Context, app string, builds []config.Build) ([]string, error)

// DeployOnly marks a PendingItem as a manifest-only redeploy (no image rebuild),
// as opposed to a non-negative build-entry index.
const DeployOnly = -1

// PendingItem is one change the manual gate holds back awaiting the user's
// decision: a dirty image build (Build is its entry index, Label its build name)
// or a manifest-only redeploy of an app (Build == DeployOnly, Label the app name).
type PendingItem struct {
	App   string
	Build int
	Label string
}

// Decision is the user's answer to one gate prompt: the items to build and deploy
// now. An empty Selected means skip — build nothing and keep watching. Reask is
// the picker reporting it was aborted (the loop asked it to, because more changes
// arrived while it was open): no decision was made, and the loop re-asks with the
// now-larger pending set. Reask and Selected are mutually exclusive.
type Decision struct {
	Selected []PendingItem
	Reask    bool
}

// Gate makes the watch loop ask before acting on incremental changes instead of
// rebuilding automatically. When set, the loop collects each change's affected
// images/apps and, once idle, hands the pending list to Ask; it then acts only on
// the items returned on Decisions. Ask must not block the loop — it kicks off the
// async picker, and exactly one prompt is outstanding at a time (the loop calls
// Ask again only after a Decision). A nil Gate keeps the classic auto-rebuild
// behavior, which is also the fallback when stdin is not an interactive terminal.
//
// Abort tells the in-flight prompt to stop: the loop calls it when fresh changes
// land while a prompt is open, so the picker tears down and the loop re-asks with
// the full set (the user sees every pending app, not just those dirty when the
// prompt first opened). The aborted picker reports Decision{Reask: true} rather
// than a selection. Abort is a no-op when no prompt is outstanding; optional (a
// gate without it simply never refreshes an open prompt).
type Gate struct {
	Ask       func([]PendingItem)
	Abort     func()
	Decisions <-chan Decision
}

// Options tune the loop; zero values get sensible watch-mode defaults.
type Options struct {
	Debounce    time.Duration // default 200ms
	MaxParallel int           // 0 = no limit (matches schedule.Options)
	RetryBase   time.Duration // default 1s
	RetryMax    time.Duration // default 2m
	Render      render.Options
	// WorkDir is the config file's directory, the value of the ${KSYNC_WORKDIR}
	// anchor when expanding an app's patch values (see config.Patch).
	WorkDir string
	Build   BuildFunc // required when any app declares builds (takeover may rebuild even an all-overridden app)
	// Overrides seeds a matching build entry with an externally-supplied image ref
	// instead of building it on first convergence: the ref is injected at deploy in
	// place of a built tag, and the entry is not built at startup (the same
	// first-time behavior as `ksync sync`). Its sources are still watched, though —
	// the first source change drops the override and the entry rebuilds from then
	// on ("takeover"), returning the watched image to the inner loop. Keyed by the
	// build entry's Image. This lets a pre-built image (a CI artifact, a registry
	// pull resolved by a wrapper) stand in until its source is touched.
	// See ADR 20260702-watch-image-override-takeover and 20260616-image-override.
	Overrides map[string]render.Image
	Log       logr.Logger
	// Report renders one app's completed sync. The loop calls it on success so
	// the watch output matches `ksync sync`'s ship-emoji apply line rather than
	// a plain log record; when nil, the loop logs a structured "synced" line.
	Report func(app string, stats SyncStats, took time.Duration)
	// OnError is called once when an app's run ends in failure (build, render, or
	// sync) and Report therefore does not fire — the command layer uses it to
	// tear down that app's live progress group so a retry starts clean. Optional.
	OnError func(app string, err error)
	// Resync, when a value is received, marks every app dirty — a programmatic
	// "redeploy everything now". A nil channel simply never fires.
	Resync <-chan struct{}
	// Gate, when set, holds incremental changes for an explicit build/deploy
	// decision instead of rebuilding automatically (see Gate). Nil keeps the
	// auto-rebuild behavior — the only option when stdin is not interactive.
	Gate *Gate
	// OnIdle, when set, is called each time the loop settles back to idle after
	// doing work — once for the initial convergence and once per rebuild batch.
	// took is the batch's wall-clock (from the loop leaving idle to returning to
	// it). The command layer uses it to print a per-batch summary and a
	// watching-for-changes line. It runs on the loop goroutine, after every app's
	// Report for the batch has fired, so it may read state those Reports updated.
	// Pure change accumulation in the manual gate never counts as work, so a held
	// or skipped change does not trigger it. Optional.
	OnIdle func(took time.Duration)
}

func (o *Options) applyDefaults() {
	if o.Debounce <= 0 {
		o.Debounce = 200 * time.Millisecond
	}
	// MaxParallel is passed through untouched: the scheduler treats 0 (or
	// negative) as no cap, so the CLI default (CPU cores) and an explicit
	// --max-parallel 0 both reach it intact.
	if o.RetryBase <= 0 {
		o.RetryBase = time.Second
	}
	if o.RetryMax <= 0 {
		o.RetryMax = 2 * time.Minute
	}
	if o.Log.GetSink() == nil {
		o.Log = logr.Discard()
	}
	if o.OnError == nil {
		o.OnError = func(string, error) {}
	}
}

// buildState is the loop's per-build-entry memory: whether sources changed
// since the last successful build, and the last built tag (re-injected on
// every render until a newer build replaces it).
type buildState struct {
	scope *build.Scope
	dirty bool
	tag   string
	// override, when set, is the externally-supplied image ref for this entry
	// (Options.Overrides): injected at deploy in place of a built tag, and not built
	// on first convergence. Its sources are still watched — the first source change
	// clears this field and the entry rebuilds from then on (takeover).
	override *render.Image
}

// Run watches the apps' inputs and builds+renders+syncs them on change until
// ctx is done. Every app is synced — and every build entry built — once at
// startup, so the cluster converges to the current working tree before
// incremental behavior takes over. Startup builds are how ksync avoids
// persisting build state: unchanged sources hit the docker layer cache and
// produce the tag already deployed.
func Run(ctx context.Context, apps []config.App, syncFn SyncFunc, opts Options) error {
	opts.applyDefaults()
	log := opts.Log
	renderer := render.New(opts.Render)

	byName := make(map[string]config.App, len(apps))
	scheduleApps := make([]schedule.App, len(apps)) // deploy phase: needs-gated
	var buildApps []schedule.App                    // build phase: no needs edges
	hasBuild := make(map[string]bool, len(apps))    // apps with ≥1 build entry (some may be overridden)
	buildable := make(map[string]bool, len(apps))   // apps with ≥1 entry to build at startup (not overridden)
	for i, a := range apps {
		byName[a.Name] = a
		scheduleApps[i] = schedule.App{Name: a.Name, Needs: a.Needs}
		for j := range a.Build {
			hasBuild[a.Name] = true
			if _, ov := opts.Overrides[a.Build[j].Image]; !ov {
				buildable[a.Name] = true
			}
		}
		// Register every build-declaring app in the build scheduler, even one whose
		// every entry is currently overridden: a later takeover (a source edit that
		// drops the override) rebuilds it locally, and MarkDirty on an unregistered
		// app is a silent no-op — which would gate that app's deploy forever, waiting
		// on a build that never schedules. For the same reason a build func is
		// required whenever an app declares builds, overridden or not.
		if hasBuild[a.Name] {
			if opts.Build == nil {
				return fmt.Errorf("app %s declares builds but no build function is configured", a.Name)
			}
			buildApps = append(buildApps, schedule.App{Name: a.Name})
		}
	}

	builds := make(map[string][]buildState, len(apps))
	deriveScopes := func(app config.App) {
		states := builds[app.Name]
		for j := range states {
			// Overridden entries are watched too, so the first source change can drop
			// the override and take the build over (see Options.Overrides).
			scope, err := build.WatchScope(app.Build[j])
			if err != nil {
				log.Error(err, "Deriving build watch scope; watching without ignore rules", "app", app.Name, "image", app.Build[j].Image)
			}
			states[j].scope = scope
		}
	}
	for _, a := range apps {
		states := make([]buildState, len(a.Build))
		for j := range states {
			if ov, ok := opts.Overrides[a.Build[j].Image]; ok {
				o := ov
				states[j].override = &o // supplied ref: deploy it, never build
			} else {
				states[j].dirty = true
			}
		}
		builds[a.Name] = states
		deriveScopes(a)
	}

	// Roots are re-derived after every run because a kustomization edit can
	// change what it references (e.g. a new chartHome), and a build's
	// .dockerignore can change what affects the image.
	appRoots := make([]watch.AppRoots, len(apps))
	rebuildRoots := func(i int) {
		app := apps[i]
		roots := []string{app.Path}
		deps, err := watch.DependencyRoots(app.Path)
		if err != nil {
			log.Error(err, "Deriving dependency roots; watching the app dir only", "app", app.Name)
		}
		appRoots[i] = watch.AppRoots{App: app.Name, Roots: append(roots, deps...)}
	}
	for i := range apps {
		rebuildRoots(i)
	}
	mappingEntries := func() []watch.AppRoots {
		entries := append([]watch.AppRoots{}, appRoots...)
		for _, a := range apps {
			for j, st := range builds[a.Name] {
				// Overridden entries are watched too — a source change takes the build
				// over — so they get a mapping entry like any other build.
				entries = append(entries, watch.AppRoots{
					App:    buildKey(a.Name, j),
					Roots:  st.scope.Roots,
					Ignore: st.scope.Ignored,
				})
			}
		}
		return entries
	}
	watchRoots := func() []watch.Root {
		var roots []watch.Root
		for _, ar := range appRoots {
			for _, r := range ar.Roots {
				roots = append(roots, watch.Root{Path: r})
			}
		}
		for _, a := range apps {
			for _, st := range builds[a.Name] {
				// Overridden entries are watched too, so their source edits reach the
				// loop and can take the build over (see Options.Overrides).
				for _, r := range st.scope.Roots {
					roots = append(roots, watch.Root{Path: r, Skip: st.scope.SkipDir})
				}
			}
		}
		return roots
	}
	mapping := watch.NewMapping(mappingEntries())

	watcher, err := watch.NewWatcher(watchRoots())
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	schedOpts := schedule.Options{
		Debounce:    opts.Debounce,
		MaxParallel: opts.MaxParallel,
		RetryBase:   opts.RetryBase,
		RetryMax:    opts.RetryMax,
	}
	// Two schedulers run the two phases independently. buildSched has no `needs`
	// edges: an image is pure-local (docker build + load into the cluster store),
	// so it builds the moment its source is dirty — overlapping the dependency
	// chain's deploys instead of waiting behind them. deploySched keeps the
	// `needs` gating (a deploy waits for its dependencies to be Healthy) plus an
	// external gate that holds it until its own image has finished building, so an
	// unbuilt tag is never deployed. Each scheduler has its own MaxParallel budget
	// — builds are the CPU/IO-heavy work, deploys are mostly health-gate waiting.
	buildSched := schedule.New(schedOpts, buildApps)
	deploySched := schedule.New(schedOpts, scheduleApps)

	// building tracks apps with a build goroutine in flight; together with any
	// still-dirty build entry it is the deploy's external gate.
	building := make(map[string]bool, len(apps))
	buildPending := func(app string) bool {
		if building[app] {
			return true
		}
		for j := range builds[app] {
			if builds[app][j].dirty {
				return true
			}
		}
		return false
	}
	deploySched.SetExternalBlock(buildPending)

	now := time.Now()
	for _, a := range apps {
		// Every app deploys once at startup to converge the cluster; a build-app's
		// deploy waits on the external gate until its startup build completes.
		deploySched.MarkDirty(a.Name, now)
		if buildable[a.Name] {
			buildSched.MarkDirty(a.Name, now)
		}
	}

	// resyncAll marks every app for a full rebuild-and-redeploy. It re-dirties each
	// still-buildable entry as well as redeploying, because the callers (a
	// programmatic Resync, a dropped-event recovery) cannot trust the dirty set: a
	// missed edit may have been a build source, so redeploying alone would ship the
	// stale tag. An overridden entry is left untouched — takeover is triggered only
	// by a real source change, never by a resync — so a resync just redeploys the
	// override. Whether to dirty the build scheduler is recomputed here from the
	// current override state rather than the startup snapshot, because takeover
	// changes an entry's override at runtime: an app that has since taken over an
	// entry must rebuild it on resync, and one still fully overridden must not
	// schedule a build (which would gate its deploy on a rebuild that never comes).
	resyncAll := func(now time.Time) {
		for _, a := range apps {
			anyBuild := false
			for j := range builds[a.Name] {
				if builds[a.Name][j].override == nil {
					builds[a.Name][j].dirty = true
					anyBuild = true
				}
			}
			if anyBuild {
				buildSched.MarkDirty(a.Name, now)
			}
			deploySched.MarkDirty(a.Name, now)
		}
	}

	rn := &runner{
		renderer: renderer,
		lookup:   render.NewVarLookup(opts.WorkDir),
		syncFn:   syncFn,
		report:   opts.Report,
		onError:  opts.OnError,
		log:      log,
	}

	buildResults := make(chan buildResult)
	deployResults := make(chan deployResult)
	var wg sync.WaitGroup
	defer wg.Wait()

	// refreshWatch re-derives the change→app/build mapping and the watched roots
	// after a phase completes: a render can change what a kustomization
	// references, and a build's .dockerignore can change what affects its image.
	refreshWatch := func() {
		mapping = watch.NewMapping(mappingEntries())
		if err := watcher.SetRoots(watchRoots()); err != nil {
			log.Error(err, "Re-deriving watch roots")
		}
	}

	// Manual gate state. Incremental changes accumulate here (instead of marking
	// the schedulers dirty) until the user picks what to build; startup
	// convergence is unaffected (it marks the schedulers directly above).
	gate := opts.Gate
	var gateDecisions <-chan Decision
	if gate != nil {
		gateDecisions = gate.Decisions
	}
	pendingBuilds := make(map[string]map[int]bool) // app -> dirty build-entry set
	pendingDeploy := make(map[string]bool)         // app -> manifest-only redeploy pending
	prompting := false                             // a prompt is outstanding (awaiting a Decision)
	abortPending := false                          // changes arrived during a prompt; abort & re-ask once the burst settles
	dismissed := false                             // user skipped; do not re-ask until a new change
	var promptDeadline time.Time                   // debounce: prompt only after the change burst settles

	// Busy-span tracking: a batch begins when the loop leaves idle (work starts)
	// and ends when it returns to idle, at which point OnIdle reports the span.
	busy := false
	var busySince time.Time

	pendingEmpty := func() bool {
		for _, m := range pendingBuilds {
			if len(m) > 0 {
				return false
			}
		}
		return len(pendingDeploy) == 0
	}
	// pendingItems is the ordered prompt list: a 🔨 row per dirty image, then a 🚢
	// row for an app whose manifests changed with no image pending (a built app
	// redeploys anyway, so that row would be redundant).
	pendingItems := func() []PendingItem {
		var items []PendingItem
		for _, a := range apps {
			m := pendingBuilds[a.Name]
			for j := range a.Build {
				if m[j] {
					items = append(items, PendingItem{App: a.Name, Build: j, Label: a.Build[j].Name})
				}
			}
			if pendingDeploy[a.Name] && len(m) == 0 {
				items = append(items, PendingItem{App: a.Name, Build: DeployOnly, Label: a.Name})
			}
		}
		return items
	}
	gateIdle := func() bool { return buildSched.Idle() && deploySched.Idle() }
	// maybePrompt hands the pending list to the gate once the loop is idle and the
	// change burst has settled, exactly once per batch (prompting guards re-asks).
	maybePrompt := func(now time.Time) {
		if gate == nil || prompting || dismissed || pendingEmpty() || !gateIdle() || now.Before(promptDeadline) {
			return
		}
		items := pendingItems()
		if len(items) == 0 {
			return
		}
		prompting = true
		gate.Ask(items)
	}
	// release acts on one chosen item: a build re-dirties its entry and schedules
	// the build plus the deploy (gated on the build); a deploy-only schedules the
	// redeploy. Either way the app's pending manifest flag clears (the redeploy
	// covers it).
	release := func(it PendingItem, now time.Time) {
		if it.Build == DeployOnly {
			delete(pendingDeploy, it.App)
			deploySched.MarkDirty(it.App, now)
			return
		}
		if m := pendingBuilds[it.App]; m != nil {
			delete(m, it.Build)
			if len(m) == 0 {
				delete(pendingBuilds, it.App)
			}
		}
		// Confirming a build entry also takes over any override on it: the user chose
		// to rebuild this image, so drop the supplied ref and build from source now
		// (this and later deploys inject the built tag). A nil override is a no-op.
		builds[it.App][it.Build].override = nil
		builds[it.App][it.Build].dirty = true
		buildSched.MarkDirty(it.App, now)
		deploySched.MarkDirty(it.App, now)
		delete(pendingDeploy, it.App)
	}

	for {
		now := time.Now()
		for _, name := range buildSched.StartDue(now) {
			app := byName[name]
			states := builds[name]
			// Snapshot the dirty entries under the loop goroutine; the build
			// goroutine must not touch shared state.
			var todo []int
			for j := range states {
				if states[j].dirty {
					todo = append(todo, j)
					states[j].dirty = false
				}
			}
			building[name] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				built, err := BuildAll(ctx, app, todo, opts.Build, opts.MaxParallel)
				r := buildResult{app: name, built: built, ok: err == nil}
				if err != nil {
					r.failed = unbuilt(todo, built)
					opts.OnError(name, err)
				}
				select {
				case buildResults <- r:
				case <-ctx.Done():
				}
			}()
		}
		for _, name := range deploySched.StartDue(now) {
			app := byName[name]
			states := builds[name]
			// Build the deploy's image set under the loop goroutine (the deploy
			// goroutine must not touch shared state): an overridden entry injects
			// its supplied ref, a built one its remembered tag.
			images := make([]render.Image, 0, len(states))
			for j := range states {
				switch {
				case states[j].override != nil:
					images = append(images, *states[j].override)
				case states[j].tag != "":
					images = append(images, render.Image{Name: app.Build[j].Image, NewTag: states[j].tag})
				}
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := rn.runDeploy(ctx, app, images)
				select {
				case deployResults <- r:
				case <-ctx.Done():
				}
			}()
		}

		// Report each busy→idle transition (a convergence or rebuild batch settling)
		// before re-prompting, so a batch summary lands ahead of any remainder
		// prompt. A change merely held by the gate never sets busy, so it does not
		// trigger a spurious "finished".
		if gateIdle() {
			if busy {
				busy = false
				if opts.OnIdle != nil {
					opts.OnIdle(now.Sub(busySince))
				}
			}
		} else if !busy {
			busy = true
			busySince = now
		}

		maybePrompt(now)
		// Changes landed while a prompt was open: once their burst settles, abort the
		// stale prompt so the loop re-asks (gateDecisions delivers Reask) with the full
		// pending set — the user sees every app, not just those dirty when it opened.
		if prompting && abortPending && !now.Before(promptDeadline) {
			abortPending = false
			if gate.Abort != nil {
				gate.Abort()
			}
		}

		var timerC <-chan time.Time
		deadline, ok := earliestDeadline(buildSched, deploySched)
		// When the gate is armed and idle, also wake at the prompt deadline so the
		// menu appears once a change burst settles, even with no scheduler work due —
		// and likewise when a prompt is open with changes to fold in (the abort above).
		if gate != nil && !dismissed && !pendingEmpty() && gateIdle() && (!prompting || abortPending) {
			if !ok || promptDeadline.Before(deadline) {
				deadline, ok = promptDeadline, true
			}
		}
		if ok {
			timerC = time.After(time.Until(deadline))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-opts.Resync:
			resyncAll(time.Now())
			log.Info("Manual resync requested", "apps", len(apps))
		case dec := <-gateDecisions:
			now := time.Now()
			prompting = false
			abortPending = false
			if dec.Reask {
				// The prompt was aborted to fold in changes that arrived while it was
				// open: act on nothing, leave the pending set intact, and let maybePrompt
				// re-ask immediately (the burst already settled to trigger the abort).
				log.V(1).Info("build prompt re-asked with updated changes")
				break
			}
			if len(dec.Selected) == 0 {
				// Skip: keep the pending set but stop asking until a new change.
				dismissed = true
				log.V(1).Info("build prompt skipped")
				break
			}
			for _, it := range dec.Selected {
				release(it, now)
			}
			// Re-offer any unselected remainder once this work finishes (idle again).
			promptDeadline = now
		case path, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			now := time.Now()
			changed := false
			for _, key := range mapping.AffectedBy(path) {
				name, entry, isBuild := parseKey(key)
				if gate != nil {
					// Manual gate: hold the change for the user's decision instead of
					// scheduling it. A build dirties its entry's pending set; any other
					// change marks a manifest-only redeploy.
					if isBuild {
						log.V(1).Info("source change held for confirmation", "path", path, "app", name, "image", byName[name].Build[entry].Image)
						if pendingBuilds[name] == nil {
							pendingBuilds[name] = map[int]bool{}
						}
						pendingBuilds[name][entry] = true
					} else {
						log.V(1).Info("change held for confirmation", "path", path, "app", name)
						pendingDeploy[name] = true
					}
					changed = true
					continue
				}
				if isBuild {
					log.V(1).Info("source change detected", "path", path, "app", name, "image", byName[name].Build[entry].Image)
					// A source change to an overridden entry takes over: drop the
					// supplied ref so this and later deploys use the freshly built tag.
					builds[name][entry].override = nil
					builds[name][entry].dirty = true
					buildSched.MarkDirty(name, now)
				} else {
					log.V(1).Info("change detected", "path", path, "app", name)
				}
				// Either way the app must re-deploy; the external gate makes the
				// deploy wait when a rebuild is also pending.
				deploySched.MarkDirty(name, now)
			}
			if changed {
				// A new change re-arms the prompt (clears a prior skip) and restarts
				// the debounce so a burst coalesces into one menu.
				dismissed = false
				promptDeadline = now.Add(opts.Debounce)
				// If a prompt is already open, it is now stale — mark it for abort so
				// the loop re-asks with this change included once the burst settles.
				if prompting {
					abortPending = true
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				// The watcher closed its error channel: its run goroutine returns
				// after closing either channel, so nothing more will arrive. Exit
				// rather than spin on the now-closed channel (a busy loop).
				return nil
			}
			// A watch error (typically an fsnotify buffer overflow) may have dropped
			// events, so the dirty set can no longer be trusted. Warn visibly and
			// resync every app — rebuilding buildable ones, since a dropped event may
			// have been a build source — so a missed edit still reaches the cluster.
			log.Error(err, "watch error; some changes may have been missed — resyncing all apps")
			resyncAll(time.Now())
		case r := <-buildResults:
			building[r.app] = false
			states := builds[r.app]
			// Tags from successful batches stick; failed entries re-dirty so the
			// build scheduler retries them (the deploy stays gated meanwhile).
			for j, tag := range r.built {
				states[j].tag = tag
			}
			for _, j := range r.failed {
				states[j].dirty = true
			}
			buildSched.Finish(r.app, r.ok, time.Now())
			// A build's .dockerignore may have changed what affects the image.
			for _, a := range apps {
				if a.Name == r.app {
					deriveScopes(a)
				}
			}
			refreshWatch()
		case r := <-deployResults:
			attempts, retryIn := deploySched.Finish(r.app, r.ok, time.Now())
			if !r.ok {
				// The whole app failed and the scheduler queued a retry: announce it
				// with the attempt number and backoff so a silent failure is visible,
				// keeping the error text on the line for grep-ability.
				log.Info(fmt.Sprintf("%s: sync failed (attempt %d); retrying in %s: %v", r.app, attempts, retryIn, r.err))
			}
			// The render may have changed what the app references.
			for i, a := range apps {
				if a.Name == r.app {
					rebuildRoots(i)
				}
			}
			refreshWatch()
		case <-timerC:
			// A debounce or retry deadline passed; StartDue above picks it up.
		}
	}
}

// earliestDeadline returns the soonest NextDeadline across the schedulers, so a
// single timer wakes the loop for whichever phase is due first.
func earliestDeadline(scheds ...*schedule.Scheduler) (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, s := range scheds {
		if d, ok := s.NextDeadline(); ok && (!found || d.Before(earliest)) {
			earliest = d
			found = true
		}
	}
	return earliest, found
}

// buildResult reports one app's build phase: the dev tag per built entry
// (recorded even when a later batch fails, so a good build is never wasted) and
// the entries whose build must run again.
type buildResult struct {
	app    string
	ok     bool
	built  map[int]string // entry -> dev tag
	failed []int          // entries whose build must run again
}

// deployResult reports one app's deploy phase (render + inject + sync). On
// failure err carries the cause, which the loop surfaces once at the scheduler's
// Finish as the retry line — runDeploy does not log it, so it is not doubled.
type deployResult struct {
	app string
	ok  bool
	err error
}

// unbuilt returns the todo entries that built does not record — what a build
// failure must re-dirty so the scheduler retries them.
func unbuilt(todo []int, built map[int]string) []int {
	var failed []int
	for _, j := range todo {
		if _, ok := built[j]; !ok {
			failed = append(failed, j)
		}
	}
	return failed
}

// runner holds the per-deploy collaborators so one scheduled deploy does not
// thread half a dozen parameters; the loop builds it once and reuses it.
type runner struct {
	renderer *render.Renderer
	lookup   func(string) (string, bool)
	syncFn   SyncFunc
	report   func(app string, stats SyncStats, took time.Duration)
	onError  func(app string, err error)
	log      logr.Logger
}

// runDeploy is one deploy of one app: render, inject the image refs (built dev
// tags and/or supplied overrides), sync. The build phase runs separately and
// ahead of this, and the deploy scheduler's external gate holds the deploy until
// the app's images have built — so an image absent from images here is a
// build-less, non-overridden one, and the manifest's own pin is used.
//
// A failure returns its cause in deployResult.err rather than logging it: the
// loop reports it once, at the scheduler's Finish, as the retry line (attempt
// count + backoff), so a single line names both what broke and when it retries.
func (rn *runner) runDeploy(ctx context.Context, app config.App, images []render.Image) deployResult {
	started := time.Now()
	res, err := rn.renderer.Render(app.Path, app.ClientRender)
	if err != nil {
		rn.onError(app.Name, err)
		return deployResult{app: app.Name, err: err}
	}
	// Patches before image injection (deploy-environment fields the kustomization
	// can't carry); SetImages must win on the built dev tag.
	if err := res.ApplyPatches(app.Patches, rn.lookup); err != nil {
		rn.onError(app.Name, err)
		return deployResult{app: app.Name, err: err}
	}
	if len(images) > 0 {
		if err := res.SetImages(images); err != nil {
			rn.onError(app.Name, err)
			return deployResult{app: app.Name, err: err}
		}
	}
	stats, err := rn.syncFn(ctx, app.Name, res.Objects)
	if err != nil {
		rn.onError(app.Name, err)
		return deployResult{app: app.Name, err: err}
	}
	rn.reportSync(app.Name, stats, time.Since(started))
	return deployResult{app: app.Name, ok: true}
}

// reportSync emits one app's completed-sync line: the injected ship-emoji
// renderer when set (matching `ksync sync`'s apply line), else a structured
// log record. applied=0 on a no-op; pruned/failed/degraded show only when
// nonzero so the common line stays short.
func (rn *runner) reportSync(app string, stats SyncStats, took time.Duration) {
	if rn.report != nil {
		rn.report(app, stats, took)
		return
	}
	kv := []any{"app", app, "applied", stats.Applied}
	if stats.Pruned > 0 {
		kv = append(kv, "pruned", stats.Pruned)
	}
	if stats.Failed > 0 {
		kv = append(kv, "failed", stats.Failed)
	}
	if stats.Degraded > 0 {
		kv = append(kv, "degraded", stats.Degraded)
	}
	kv = append(kv, "took", ui.Duration(took))
	rn.log.Info("Synced", kv...)
}

// BuildAll builds every batch of app's dirty entries (todo), up to maxParallel
// batches at once (<=0 means no limit), and returns each built entry's dev tag.
// A batch — one bulk group command or one ungrouped entry — is independent of
// the others, so they build concurrently: a single multi-image app's builds no
// longer run one at a time, which is the bulk of its change→applied latency.
// On the first batch failure it cancels the rest and returns the tags built so
// far plus that error, so the caller re-dirties only what did not build. Build
// progress and failures are surfaced by the injected BuildFunc (a build/import
// row per batch in the app's pipeline, full log only on failure).
func BuildAll(ctx context.Context, app config.App, todo []int, buildFn BuildFunc, maxParallel int) (map[int]string, error) {
	batches := app.BuildBatches(todo)
	built := make(map[int]string, len(todo))
	if len(batches) == 0 {
		return built, nil
	}
	var sem chan struct{}
	if maxParallel > 0 {
		sem = make(chan struct{}, maxParallel)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for _, batch := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sem != nil {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
			}
			builds := make([]config.Build, len(batch))
			for k, j := range batch {
				builds[k] = app.Build[j]
			}
			refs, err := buildFn(ctx, app.Name, builds)
			if err != nil {
				errOnce.Do(func() { firstErr = err; cancel() })
				return
			}
			mu.Lock()
			for k, j := range batch {
				built[j] = build.Tag(refs[k])
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return built, firstErr
}

// keySep joins app name and build-entry index into one mapping key; NUL can
// never appear in an app name (names are Kubernetes label values).
const keySep = "\x00"

func buildKey(app string, entry int) string {
	return app + keySep + strconv.Itoa(entry)
}

func parseKey(key string) (app string, entry int, isBuild bool) {
	app, num, found := strings.Cut(key, keySep)
	if !found {
		return key, 0, false
	}
	entry, _ = strconv.Atoi(num)
	return app, entry, true
}
