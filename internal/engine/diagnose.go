package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
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
	diagMaxPods      = 4  // related pods detailed per run
	diagMaxEvents    = 6  // most-recent events kept per object
	diagLogTailLines = 25 // log lines tailed per container stream
)

// Diagnose gathers a compact debug dump for an app whose sync timed out before
// becoming healthy: each still-unhealthy managed resource with its recent
// events, and the related pods (found by walking the warm cache's ownership
// hierarchy) with their container state and current + previous log tails. It
// returns the block as plain lines (nil when nothing is unhealthy).
//
// The read paths hit the API server (events, pod get, pod logs), so it must run
// with a live, bounded context — never the sync's own timed-out one, which
// would make every call return at once and the dump come back empty.
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
		// reason — that have no pod row of their own.
		if rs.Kind != "Pod" {
			rd.events = e.recentEvents(ctx, rs.Kind, rs.Namespace, rs.Name)
		}
		resources = append(resources, rd)
	}

	var pods []podDiag
	for _, key := range e.relatedPods(keysOf(unhealthy)) {
		if len(pods) >= diagMaxPods {
			break
		}
		pods = append(pods, e.podDiag(ctx, key))
	}
	return formatDiagnostics(resources, pods)
}

// resourceDiag is one unhealthy managed resource: its health verdict and (for
// non-pod kinds) its recent events.
type resourceDiag struct {
	status ResourceStatus
	events []string
}

// podDiag is one related pod's debug context: its phase, per-container state,
// recent events, and the tail of its current and (when it restarted) previous
// logs.
type podDiag struct {
	namespace, name string
	phase           string
	containers      []string
	events          []string
	logs            string
	prevLogs        string
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

// relatedPods walks the warm cache's ownership hierarchy down from the given
// top-level resources and returns the keys of every Pod beneath them (a
// Deployment's ReplicaSet's pods, a hook Job's pods, or a bare pod itself),
// deduplicated and sorted. It reads only the cache (ownerRef index), so it costs
// no API calls. Returning true from the callback is what makes the walk descend
// into a resource's children.
func (e *Engine) relatedPods(roots []kube.ResourceKey) []kube.ResourceKey {
	seen := map[kube.ResourceKey]bool{}
	var pods []kube.ResourceKey
	e.clusterCache.IterateHierarchyV2(roots, func(r *cache.Resource, _ map[kube.ResourceKey]*cache.Resource) bool {
		if r.Ref.Kind == "Pod" {
			key := r.ResourceKey()
			if !seen[key] {
				seen[key] = true
				pods = append(pods, key)
			}
		}
		return true // descend into children
	})
	sort.Slice(pods, func(i, j int) bool { return pods[i].String() < pods[j].String() })
	return pods
}

// podDiag fetches one pod's status and logs. The cache holds full manifests only
// for ksync-managed resources, and controller-spawned pods are not managed, so
// the pod is read live here. Errors degrade gracefully — a missing previous log
// (no prior run) or an unreadable pod is simply omitted, never fatal.
func (e *Engine) podDiag(ctx context.Context, key kube.ResourceKey) podDiag {
	d := podDiag{namespace: key.Namespace, name: key.Name}
	pod, err := e.kclient.CoreV1().Pods(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		d.phase = fmt.Sprintf("unreadable: %v", err)
		return d
	}
	d.phase = string(pod.Status.Phase)
	// Pod events carry the only signal for a pod that never ran a container —
	// FailedScheduling, FailedMount — which leaves no container state or logs.
	d.events = e.recentEvents(ctx, "Pod", key.Namespace, key.Name)
	logContainer, restarted := "", false
	for _, cs := range pod.Status.ContainerStatuses {
		d.containers = append(d.containers, containerState(cs))
		// Tail the most telling container: a waiting/terminated one if present,
		// else the first. CrashLoops and ImagePullBackOffs land here.
		if logContainer == "" || isProblemContainer(cs) {
			logContainer = cs.Name
		}
		if cs.RestartCount > 0 {
			restarted = true
		}
	}
	if logContainer != "" {
		d.logs = e.podLogs(ctx, key.Namespace, key.Name, logContainer, false)
		if restarted {
			d.prevLogs = e.podLogs(ctx, key.Namespace, key.Name, logContainer, true)
		}
	}
	return d
}

// containerState summarizes one container's runtime state for the pod row —
// the waiting/terminated reason is usually the whole story (ErrImagePull,
// CrashLoopBackOff, OOMKilled).
func containerState(cs corev1.ContainerStatus) string {
	switch {
	case cs.State.Waiting != nil:
		return fmt.Sprintf("%s: Waiting (%s)", cs.Name, cs.State.Waiting.Reason)
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
// waiting, terminated, not-ready, or having restarted.
func isProblemContainer(cs corev1.ContainerStatus) bool {
	return cs.State.Waiting != nil || cs.State.Terminated != nil ||
		(cs.State.Running != nil && !cs.Ready) || cs.RestartCount > 0
}

// podLogs returns the tail of one container's log stream (the previous instance
// when previous is set, for a crashed container). An empty string on any error —
// previous logs are commonly absent, and a failed read must not break the dump.
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
	return strings.TrimRight(b.String(), "\n")
}

// recentEvents returns the most recent events for one object, newest last, as
// "Type Reason: message" lines. Scoped by namespace, name, and kind so a
// same-named object of another kind does not bleed in. Empty on any error or
// when the object has none.
func (e *Engine) recentEvents(ctx context.Context, kind, ns, name string) []string {
	sel := fields.Set{
		"involvedObject.name": name,
		"involvedObject.kind": kind,
	}.AsSelector().String()
	list, err := e.kclient.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel, Limit: 50})
	if err != nil || len(list.Items) == 0 {
		return nil
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return eventTime(items[i]).Before(eventTime(items[j])) })
	if len(items) > diagMaxEvents {
		items = items[len(items)-diagMaxEvents:] // keep the most recent
	}
	out := make([]string, 0, len(items))
	for _, ev := range items {
		line := fmt.Sprintf("%s %s: %s", ev.Type, ev.Reason, strings.TrimSpace(ev.Message))
		if ev.Count > 1 {
			line += fmt.Sprintf(" (x%d)", ev.Count)
		}
		out = append(out, line)
	}
	return out
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

// formatDiagnostics renders the gathered resource and pod diagnostics as an
// indented block. Pure (no I/O), so the layout is unit-tested directly.
func formatDiagnostics(resources []resourceDiag, pods []podDiag) []string {
	var out []string
	for _, d := range resources {
		out = append(out, "  "+d.status.Line())
		for _, ev := range d.events {
			out = append(out, "      "+ev)
		}
	}
	for _, p := range pods {
		out = append(out, fmt.Sprintf("  pod %s/%s: %s", p.namespace, p.name, p.phase))
		for _, c := range p.containers {
			out = append(out, "      "+c)
		}
		for _, ev := range p.events {
			out = append(out, "      "+ev)
		}
		out = append(out, logBlock("logs", p.logs)...)
		out = append(out, logBlock("logs (previous)", p.prevLogs)...)
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
