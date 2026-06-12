package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/cache"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/sync/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SyncOptions tune one Sync call.
type SyncOptions struct {
	// Prune deletes tracked resources of the app that are absent from the
	// target set.
	Prune bool
	// Namespace is applied to namespaced resources that carry none.
	Namespace string
}

// Sync makes the cluster state of one app match the given rendered resources,
// with ArgoCD semantics: server-side apply, hook phases and waves from the
// resource annotations, health-gated PostSync, and tracking-label-scoped
// prune.
func (e *Engine) Sync(ctx context.Context, app string, resources []*unstructured.Unstructured, opts SyncOptions) ([]common.ResourceSyncResult, error) {
	target := StampTracking(app, resources)
	return e.engine.Sync(ctx, target,
		func(r *cache.Resource) bool {
			info, ok := r.Info.(*resourceInfo)
			return ok && info.app == app
		},
		revision(target),
		opts.Namespace,
		sync.WithLogr(e.log),
		sync.WithPrune(opts.Prune),
		// Production parity: the reference ArgoCD setup applies everything
		// server-side.
		sync.WithServerSideApply(true),
		sync.WithServerSideApplyManager(fieldManager),
	)
}

// StampTracking returns copies of objs labeled as belonging to app. Copies,
// because callers reuse the rendered objects (e.g. for diff output).
func StampTracking(app string, objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, len(objs))
	for i, obj := range objs {
		c := obj.DeepCopy()
		labels := c.GetLabels()
		if labels == nil {
			labels = make(map[string]string, 1)
		}
		labels[TrackingLabel] = app
		c.SetLabels(labels)
		out[i] = c
	}
	return out
}

// revision identifies the synced content in results and logs. ksync syncs
// working trees, not commits, so the "revision" is a content hash of the
// target manifests (json.Marshal sorts map keys, making it deterministic).
func revision(objs []*unstructured.Unstructured) string {
	h := sha256.New()
	for _, o := range objs {
		b, _ := json.Marshal(o.Object)
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
