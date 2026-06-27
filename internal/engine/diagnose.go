package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/cache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
)

// Diagnostic bounds: a timeout dump is a debugging aid, not a log export, so
// every axis is capped hard to stay scannable — ksync is not a debug-first tool.
const (
	diagMaxPods      = 4  // distinct pod failures detailed per run (after dedup)
	diagMaxPodScan   = 20 // related pods inspected per run (live GETs/logs) before dedup
	diagMaxEvents    = 4  // warning events kept per object (after dedup)
	diagLogTailLines = 20 // log lines tailed for the one chosen container
)

// Diagnose gathers a compact debug dump for an app whose sync timed out before
// becoming healthy: each still-unhealthy managed resource with its warning
// events, and the related pods (found by walking the warm cache's ownership
// hierarchy) with their container state, warning events, and the tail of the one
// container worth reading. Identical pods (a Deployment's failing replicas) are
// collapsed to a single representative. It returns the block as plain lines (nil
// when nothing is unhealthy).
//
// The read paths hit the API server (events, pod get, pod logs), so it must run
// with a live, bounded context — never the sync's own cancelled one, which would
// make every call return at once and the dump come back empty.
func (e *Engine) Diagnose(ctx context.Context, app, namespace string, objects []*unstructured.Unstructured) []string {
	target := StampTracking(app, objects)
	if namespace != "" {
		fillDefaultNamespace(target, namespace, e.clusterCache.IsNamespaced)
	}
	isManaged := func(r *cache.Resource) bool {
		info, ok := r.Info.(*resourceInfo)
		return ok && info.app == app
	}
	lives, err := e.clusterCache.GetManagedLiveObjs(target, isManaged)
	if err != nil {
		return nil
	}
	unhealthy := unhealthyStatuses(lives)
	if len(unhealthy) == 0 {
		return nil
	}

	resources := make([]resourceDiag, 0, len(unhealthy))
	for _, rs := range unhealthy {
		rd := resourceDiag{status: rs}
		// A Pod's own events surface again in its pod section below (where its
		// logs are), so gather resource-level events only for controllers and other
		// non-pod kinds — the Deployment's ProgressDeadlineExceeded, a PVC's pending
		// reason — that have no pod row of their own. The status line is the signal,
		// so only warnings add to it (no Normal-event fallback).
		if rs.Kind != "Pod" {
			rd.events = displayEvents(e.recentEvents(ctx, rs.Kind, rs.Namespace, rs.Name), true)
		}
		resources = append(resources, rd)
	}

	related, controllers := e.relatedResources(keysOf(unhealthy))

	// Intermediate controllers (a ReplicaSet, a Job) between a root and its pods are
	// shown only when they carry a warning the pods cannot — a FailedCreate when no
	// pod could be created at all (ResourceQuota, admission webhook). Warnings-only
	// keeps a healthy controller (an old scaled-down ReplicaSet) silent. Their count
	// is bounded by rollout history, not replica count, so no scan cap is needed.
	for _, key := range controllers {
		c := statusFromKey(key)
		if evs := displayEvents(e.recentEvents(ctx, c.Kind, c.Namespace, c.Name), true); len(evs) > 0 {
			resources = append(resources, resourceDiag{status: c, events: evs})
		}
	}

	uninspected := 0
	if len(related) > diagMaxPodScan {
		// podDiag does live GETs, an event list, and a log stream per pod, so an app
		// with a large failing ReplicaSet (or several interrupted at once) could fan
		// out into many API calls and sit on the whole gather budget — to print at
		// most diagMaxPods of them. Bound the scan and note what was skipped rather
		// than inspect every replica.
		uninspected = len(related) - diagMaxPodScan
		related = related[:diagMaxPodScan]
	}
	var pods []podDiag
	for _, key := range related {
		pods = append(pods, e.podDiag(ctx, key))
	}
	pods = dedupePods(pods)
	omittedModes := 0
	if len(pods) > diagMaxPods {
		// Each retained pod is a distinct failure (dedupePods already merged the
		// identical ones), so a silent truncation here drops a real failure mode.
		// Surface the count rather than imply the dump is exhaustive.
		omittedModes = len(pods) - diagMaxPods
		pods = pods[:diagMaxPods]
	}
	return formatDiagnostics(resources, pods, omittedModes, uninspected)
}

// resourceDiag is one unhealthy managed resource: its health verdict and (for
// non-pod kinds) its recent warning events.
type resourceDiag struct {
	status ResourceStatus
	events []string
}

// podDiag is one related pod's debug context: its phase, per-container state,
// warning events, and the tail of the one container worth reading. dupes counts
// how many further pods collapsed into this one as identical (see dedupePods).
type podDiag struct {
	namespace, name string
	phase           string
	containers      []string
	events          []string
	logLabel        string // "logs" or "logs (previous run)"
	logs            string
	dupes           int
}

// keysOf returns the cache resource keys of the given statuses, for seeding the
// hierarchy walk.
func keysOf(ss []ResourceStatus) []kube.ResourceKey {
	keys := make([]kube.ResourceKey, len(ss))
	for i, s := range ss {
		keys[i] = s.key()
	}
	return keys
}

// relatedResources walks the warm cache's ownership hierarchy down from the given
// top-level resources and returns, separately, every Pod beneath them (a
// Deployment's ReplicaSet's pods, a hook Job's pods, or a bare pod itself) and the
// intermediate controllers between a root and its pods (a ReplicaSet, a Job). The
// roots themselves are excluded from the controllers (the caller already reports
// them). Both lists are deduplicated and sorted. It reads only the cache (ownerRef
// index), so it costs no API calls; returning true from the callback is what makes
// the walk descend into a resource's children.
//
// The controllers matter because when one cannot create its pods at all — a
// ResourceQuota or admission webhook denying creation — the only actionable signal
// (a FailedCreate warning) sits on the controller, with no pod to walk to.
func (e *Engine) relatedResources(roots []kube.ResourceKey) (pods, controllers []kube.ResourceKey) {
	rootSet := make(map[kube.ResourceKey]bool, len(roots))
	for _, k := range roots {
		rootSet[k] = true
	}
	seen := map[kube.ResourceKey]bool{}
	e.clusterCache.IterateHierarchyV2(roots, func(r *cache.Resource, _ map[kube.ResourceKey]*cache.Resource) bool {
		key := r.ResourceKey()
		if seen[key] {
			return true
		}
		seen[key] = true
		switch {
		case r.Ref.Kind == "Pod":
			pods = append(pods, key)
		case !rootSet[key]:
			controllers = append(controllers, key)
		}
		return true // descend into children
	})
	sort.Slice(pods, func(i, j int) bool { return pods[i].String() < pods[j].String() })
	sort.Slice(controllers, func(i, j int) bool { return controllers[i].String() < controllers[j].String() })
	return pods, controllers
}

// statusFromKey is a ResourceStatus carrying only identity (no health verdict),
// for an intermediate controller surfaced solely to show its warning events.
func statusFromKey(k kube.ResourceKey) ResourceStatus {
	return ResourceStatus{Group: k.Group, Kind: k.Kind, Namespace: k.Namespace, Name: k.Name}
}

// podDiag fetches one pod's status and logs. The cache holds full manifests only
// for ksync-managed resources, and controller-spawned pods are not managed, so
// the pod is read live here. Errors degrade gracefully — a failed read leaves the
// row with whatever it has, never fatal.
func (e *Engine) podDiag(ctx context.Context, key kube.ResourceKey) podDiag {
	d := podDiag{namespace: key.Namespace, name: key.Name}
	pod, err := e.kclient.CoreV1().Pods(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return d // unreadable; the bare name still tells the reader which pod
	}
	d.phase = podPhase(pod)
	// Pick the one container to tail. A failing init container blocks the whole
	// pod (the app containers then sit in PodInitializing with nothing to say), so
	// it wins; else a failing app container; else the first app container. Init
	// states are listed first, prefixed, so the reader sees the startup order.
	var logC string
	var logPrev bool
	for _, cs := range pod.Status.InitContainerStatuses {
		d.containers = append(d.containers, "init "+containerState(cs))
		if logC == "" && isProblemContainer(cs) {
			logC, logPrev = cs.Name, cs.RestartCount > 0
		}
	}
	appC, firstApp := "", ""
	var appPrev bool
	for _, cs := range pod.Status.ContainerStatuses {
		d.containers = append(d.containers, containerState(cs))
		if firstApp == "" {
			firstApp = cs.Name
		}
		if appC == "" && isProblemContainer(cs) {
			appC, appPrev = cs.Name, cs.RestartCount > 0
		}
	}
	if logC == "" { // no failing init container blocking startup
		switch {
		case appC != "":
			logC, logPrev = appC, appPrev
		default:
			logC = firstApp
		}
	}

	rawEvents := e.recentEvents(ctx, "Pod", key.Namespace, key.Name)
	if logC != "" {
		d.logLabel, d.logs = e.bestLogs(ctx, key.Namespace, key.Name, logC, logPrev)
	}
	// Pod events carry the only signal for a pod that never ran a container —
	// FailedScheduling, FailedMount — so keep the single latest one as a fallback
	// when there are no warnings and no container state or logs to show.
	haveSignal := len(d.containers) > 0 || d.logs != ""
	d.events = displayEvents(rawEvents, haveSignal)
	return d
}

// podPhase is the pod's phase, prefixed with status.reason when the kubelet set
// one (Evicted, NodeAffinity, Shutdown) — the reason is the actionable word.
func podPhase(pod *corev1.Pod) string {
	if pod.Status.Reason != "" {
		if msg := strings.TrimSpace(pod.Status.Message); msg != "" {
			return fmt.Sprintf("%s (%s: %s)", pod.Status.Phase, pod.Status.Reason, msg)
		}
		return fmt.Sprintf("%s (%s)", pod.Status.Phase, pod.Status.Reason)
	}
	return string(pod.Status.Phase)
}

// containerState summarizes one container's runtime state for the pod row — the
// waiting/terminated reason is usually the whole story (ErrImagePull,
// CrashLoopBackOff, OOMKilled). A waiting container that previously died (a
// CrashLoopBackOff) carries its real cause in the last-termination state, not the
// bland "CrashLoopBackOff", so that is appended.
func containerState(cs corev1.ContainerStatus) string {
	switch {
	case cs.State.Waiting != nil:
		s := fmt.Sprintf("%s: Waiting (%s)", cs.Name, cs.State.Waiting.Reason)
		if t := cs.LastTerminationState.Terminated; t != nil {
			s += fmt.Sprintf("; last: Terminated (%s, exit %d)", t.Reason, t.ExitCode)
		}
		return s
	case cs.State.Terminated != nil:
		t := cs.State.Terminated
		return fmt.Sprintf("%s: Terminated (%s, exit %d)", cs.Name, t.Reason, t.ExitCode)
	case cs.State.Running != nil:
		ready := "not ready"
		if cs.Ready {
			ready = "ready"
		}
		return fmt.Sprintf("%s: Running (%s, restarts %d)", cs.Name, ready, cs.RestartCount)
	default:
		return cs.Name + ": Unknown"
	}
}

// isProblemContainer reports whether a container is in a state worth tailing —
// terminated, not-ready, restarted, or waiting on its own fault. A container
// merely waiting to start (PodInitializing behind a slow init container,
// ContainerCreating during image pull / volume mount) has no logs and is blocked
// on something else, so it is not the container to read.
func isProblemContainer(cs corev1.ContainerStatus) bool {
	if w := cs.State.Waiting; w != nil {
		return w.Reason != "PodInitializing" && w.Reason != "ContainerCreating"
	}
	return cs.State.Terminated != nil ||
		(cs.State.Running != nil && !cs.Ready) || cs.RestartCount > 0
}

// bestLogs returns the most informative single log stream for one container,
// with its label. For a container that has restarted it tries the previous
// instance first (the run that actually crashed); it falls back to the current
// stream when the previous is empty or a kubelet placeholder. Returning one
// stream, not both, keeps the dump short — the two are usually redundant.
func (e *Engine) bestLogs(ctx context.Context, ns, name, container string, restarted bool) (label, body string) {
	if restarted {
		if prev := e.podLogs(ctx, ns, name, container, true); prev != "" {
			return "logs (previous run)", prev
		}
	}
	return "logs", e.podLogs(ctx, ns, name, container, false)
}

// podLogs returns the tail of one container's log stream (the previous instance
// when previous is set). An empty string on any error — previous logs are
// commonly absent — and on the kubelet placeholder it emits for a log it cannot
// retrieve, which is noise, not a log.
func (e *Engine) podLogs(ctx context.Context, ns, name, container string, previous bool) string {
	tail := int64(diagLogTailLines)
	rc, err := e.kclient.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &tail,
	}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	var b strings.Builder
	sc := bufio.NewScanner(io.LimitReader(rc, 64*1024))
	for sc.Scan() {
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	body := strings.TrimRight(b.String(), "\n")
	if strings.HasPrefix(body, "unable to retrieve container logs") {
		return ""
	}
	return body
}

// eventInfo is one Kubernetes event reduced to what the dump shows.
type eventInfo struct {
	warning bool
	reason  string
	message string
	count   int
	at      time.Time
}

// recentEvents returns one object's events (oldest first), scoped by namespace,
// name, and kind so a same-named object of another kind does not bleed in. Empty
// on any error or when the object has none. Filtering and dedup happen in
// displayEvents.
func (e *Engine) recentEvents(ctx context.Context, kind, ns, name string) []eventInfo {
	sel := fields.Set{
		"involvedObject.name": name,
		"involvedObject.kind": kind,
	}.AsSelector().String()
	list, err := e.kclient.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel, Limit: 50})
	if err != nil || len(list.Items) == 0 {
		return nil
	}
	out := make([]eventInfo, 0, len(list.Items))
	for _, ev := range list.Items {
		count := int(ev.Count)
		if count == 0 {
			count = 1
		}
		out = append(out, eventInfo{
			warning: ev.Type == corev1.EventTypeWarning,
			reason:  ev.Reason,
			message: strings.TrimSpace(ev.Message),
			count:   count,
			at:      eventTime(ev),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// displayEvents reduces raw events to the lines worth showing: warnings only
// (Normal events — Scheduled, Pulled, Started — are noise), deduped so a stuck
// pod's repeated reasons collapse. When there are no warnings it shows the single
// latest event only if nothing else carries the signal (no container state or
// logs), so a pod that never started still says why. Newest last, capped.
func displayEvents(evs []eventInfo, haveSignal bool) []string {
	var warnings []eventInfo
	for _, e := range evs {
		// A "BackOff" warning ("Back-off restarting failed container ...") only
		// restates the container's CrashLoopBackOff/ImagePullBackOff state, which the
		// dump already shows — with the real cause in last-termination and the logs —
		// while carrying a noisy pod name and UID. Drop it as redundant.
		if e.warning && e.reason != "BackOff" {
			warnings = append(warnings, e)
		}
	}
	if len(warnings) == 0 {
		if haveSignal || len(evs) == 0 {
			return nil
		}
		return []string{formatEvent(evs[len(evs)-1])} // sole signal: latest event
	}
	warnings = dedupeEvents(warnings)
	if len(warnings) > diagMaxEvents {
		warnings = warnings[len(warnings)-diagMaxEvents:]
	}
	out := make([]string, len(warnings))
	for i, e := range warnings {
		out[i] = formatEvent(e)
	}
	return out
}

// dedupeEvents collapses an object's repeated warnings: first messages that are
// the same once ids and numbers are normalized (the same reason re-firing), then,
// per reason, the single most informative message (an ImagePullBackOff fires
// "Error: ImagePullBackOff", "Error: ErrImagePull", and "Failed to pull ... no
// such host" — only the last is worth keeping). Counts are summed, newest time
// kept; the result is sorted oldest first.
func dedupeEvents(evs []eventInfo) []eventInfo {
	byMsg := map[string]*eventInfo{}
	for _, e := range evs {
		k := e.reason + "\x00" + normalizeMsg(e.message)
		if cur, ok := byMsg[k]; ok {
			cur.count += e.count
			if e.at.After(cur.at) {
				cur.at = e.at
			}
			if len(e.message) > len(cur.message) {
				cur.message = e.message
			}
			continue
		}
		c := e
		byMsg[k] = &c
	}
	byReason := map[string]*eventInfo{}
	for _, e := range byMsg {
		if cur, ok := byReason[e.reason]; !ok || len(e.message) > len(cur.message) {
			merged := *e
			if ok {
				merged.count += cur.count
				if cur.at.After(merged.at) {
					merged.at = cur.at
				}
			}
			byReason[e.reason] = &merged
		} else {
			cur.count += e.count
			if e.at.After(cur.at) {
				cur.at = e.at
			}
		}
	}
	out := make([]eventInfo, 0, len(byReason))
	for _, e := range byReason {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

func formatEvent(e eventInfo) string {
	typ := "Normal"
	if e.warning {
		typ = "Warning"
	}
	line := fmt.Sprintf("%s %s: %s", typ, e.reason, e.message)
	if e.count > 1 {
		line += fmt.Sprintf(" (x%d)", e.count)
	}
	return line
}

var (
	hexRe     = regexp.MustCompile(`[0-9a-f]{8,}`)
	numRe     = regexp.MustCompile(`\d+`)
	restartRe = regexp.MustCompile(`restarts \d+`)
	uuidRe    = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// normalizeMsg strips the volatile parts of an event message (hex ids, numbers)
// so two firings of the same underlying condition group together.
func normalizeMsg(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = hexRe.ReplaceAllString(s, "#")
	s = numRe.ReplaceAllString(s, "#")
	return s
}

// eventTime is an event's most recent occurrence, preferring the series/last
// timestamps over the creation time so sorting reflects when it last fired.
func eventTime(ev corev1.Event) time.Time {
	if !ev.LastTimestamp.IsZero() {
		return ev.LastTimestamp.Time
	}
	if !ev.EventTime.IsZero() {
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}

// dedupePods collapses pods that fail the same way — a Deployment's identical
// replicas — into one representative carrying a count of the rest, so three
// CrashLooping replicas show one log tail, not three. The signature is the
// container states and warning events with the pod's own name, restart counts,
// and repeat counts normalized out, so only a genuine difference (a different
// exit reason, a different unschedulable cause) keeps pods apart.
func dedupePods(pods []podDiag) []podDiag {
	var out []podDiag
	at := map[string]int{}
	for _, p := range pods {
		sig := p.signature()
		if i, ok := at[sig]; ok {
			out[i].dupes++
			continue
		}
		at[sig] = len(out)
		out = append(out, p)
	}
	return out
}

func (p podDiag) signature() string {
	parts := make([]string, 0, len(p.containers)+len(p.events))
	// Container states keep their exit codes (a different exit is a different
	// failure), normalizing only the volatile restart count.
	for _, c := range p.containers {
		s := strings.ReplaceAll(c, p.name, "POD")
		parts = append(parts, restartRe.ReplaceAllString(s, "restarts N"))
	}
	// Event lines are normalized hard: a BackOff message embeds the pod's name,
	// UID, and an "(xN)" repeat count that all differ between otherwise-identical
	// replicas. Stripping them leaves only the semantic skeleton, so a genuinely
	// different message (a different unschedulable reason) still keeps pods apart.
	for _, ev := range p.events {
		s := strings.ReplaceAll(ev, p.name, "POD")
		s = uuidRe.ReplaceAllString(s, "#")
		s = hexRe.ReplaceAllString(s, "#")
		s = numRe.ReplaceAllString(s, "#")
		parts = append(parts, s)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// formatDiagnostics renders the gathered resource and pod diagnostics as an
// indented block. Pure (no I/O), so the layout is unit-tested directly.
func formatDiagnostics(resources []resourceDiag, pods []podDiag, omittedModes, uninspected int) []string {
	var out []string
	for _, d := range resources {
		out = append(out, "  "+d.status.Line())
		for _, ev := range d.events {
			out = append(out, "      "+ev)
		}
	}
	for _, p := range pods {
		head := fmt.Sprintf("  pod %s/%s", p.namespace, p.name)
		if p.dupes > 0 {
			head += fmt.Sprintf(" (+%d more identical)", p.dupes)
		}
		if p.phase != "" {
			head += ": " + p.phase
		}
		out = append(out, head)
		for _, c := range p.containers {
			out = append(out, "      "+c)
		}
		for _, ev := range p.events {
			out = append(out, "      "+ev)
		}
		out = append(out, logBlock(p.logLabel, p.logs)...)
	}
	if omittedModes > 0 {
		noun := "failure mode"
		if omittedModes > 1 {
			noun += "s"
		}
		out = append(out, fmt.Sprintf("  (+%d more distinct %s not shown)", omittedModes, noun))
	}
	if uninspected > 0 {
		out = append(out, fmt.Sprintf("  (+%d related pods not inspected)", uninspected))
	}
	return out
}

// logBlock indents a captured log tail under a label, or nothing when empty.
func logBlock(label, body string) []string {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	out := []string{"      " + label + ":"}
	for _, ln := range strings.Split(body, "\n") {
		out = append(out, "        "+ln)
	}
	return out
}
