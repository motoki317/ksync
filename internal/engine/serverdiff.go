package engine

import (
	"fmt"

	"github.com/argoproj/argo-cd/gitops-engine/pkg/diff"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/kube"
	"github.com/argoproj/argo-cd/gitops-engine/pkg/utils/tracing"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// differ computes one resource's diff with a chosen strategy. Server-side runs a
// dry-run server-side apply and compares its predicted result with live, so
// fields the apiserver defaults or prunes — a disabled MaxUnavailableStatefulSet
// feature gate dropping spec.updateStrategy.rollingUpdate.maxUnavailable — are
// reflected and never read as perpetual drift; client-side is the in-process
// three-way / structured-merge diff. A server-side dry-run that errors for one
// resource (a validating webhook, missing RBAC, a transient API error) falls
// back to the client-side result for that one resource, so a single resource
// never fails the whole diff or sync.
type differ struct {
	serverSide bool
	serverOpts []diff.Option // the server-side option set; nil when client-side
	// stripLabel removes the ksync tracking label before comparing — true for the
	// diff display (hide a label-only adoption), false for the sync apply-set
	// decision (an unlabeled-but-matching resource must apply, to be adopted).
	stripLabel bool
	log        logr.Logger
}

// classify computes the diff used both to decide create/update/prune and to
// render the change. serverMasked reports the single case the caller must NOT
// re-mask a Secret for display: gitops-engine's server-side diff masks Secret
// values internally, but only on its update path — a create or prune (one side
// nil) takes the shallow, unmasked diff, and a fallback to client-side is
// unmasked too. So serverMasked is true only for a server-side diff of an
// existing resource (both sides non-nil).
func (d *differ) classify(config, live *unstructured.Unstructured) (dr *diff.DiffResult, serverMasked bool, err error) {
	if d.serverSide {
		opts := append([]diff.Option{diff.WithLogr(d.log), diff.WithNormalizer(noiseNormalizer{stripLabel: d.stripLabel})}, d.serverOpts...)
		r, e := diff.Diff(config, live, opts...)
		if e == nil {
			return r, config != nil && live != nil, nil
		}
		d.log.V(1).Info("server-side diff failed; using client-side for this resource", "error", e.Error())
	}
	r, e := diff.Diff(normalizeForDiff(config, d.stripLabel), normalizeForDiff(live, d.stripLabel), diff.WithLogr(d.log))
	return r, false, e
}

// diffArray classifies an aligned target/live array — the shape
// WithResourceModificationChecker consumes to apply only out-of-sync resources.
// It mirrors gitops-engine's diff.DiffArray (per-pair Diff, OR-ing Modified) but
// routes each pair through the chosen strategy, so a sync's apply-skip decision
// matches what `ksync diff` previews. The per-resource server-side fallback keeps
// one dry-run failure from failing the whole sync.
func (d *differ) diffArray(configs, lives []*unstructured.Unstructured) (*diff.DiffResultList, error) {
	out := &diff.DiffResultList{Diffs: make([]diff.DiffResult, len(configs))}
	for i := range configs {
		dr, _, err := d.classify(configs[i], lives[i])
		if err != nil {
			return nil, err
		}
		out.Diffs[i] = *dr
		if dr.Modified {
			out.Modified = true
		}
	}
	return out, nil
}

// newDiffer builds a differ for the chosen strategy. For server-side it wires the
// dry-run applier (from the same kubectl Sync uses) and the GVK parser the warm
// cache maintains; the returned cleanup releases the applier's resources. A nil
// parser (cache not yet populated) disables webhook-mutation removal rather than
// erroring on the nil, since that path is the only consumer of the parser.
func (e *Engine) newDiffer(serverSide, stripLabel bool) (*differ, func(), error) {
	if !serverSide {
		return &differ{serverSide: false, stripLabel: stripLabel, log: e.log}, func() {}, nil
	}
	// ManageServerSideDiffDryRuns, not ManageResources: the dry-run applier it
	// returns prints the predicted object as JSON (what gitops-engine's
	// serverSideDiff unmarshals), whereas the general ManageResources applier
	// prints kubectl's "serverside-applied (server dry run)" status line, which
	// fails to unmarshal and silently degrades every resource to client-side.
	applier, cleanup, err := kube.ManageServerSideDiffDryRuns(e.cfg, e.clusterCache.GetOpenAPISchema(), tracing.NopTracer{}, e.log, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing server-side diff: %w", err)
	}
	opts := []diff.Option{
		diff.WithServerSideDiff(true),
		diff.WithServerSideDryRunner(diff.NewK8sServerSideDryRunner(applier)),
		diff.WithManager(fieldManager),
	}
	if parser := e.clusterCache.GetGVKParser(); parser != nil {
		opts = append(opts, diff.WithGVKParser(parser))
	} else {
		opts = append(opts, diff.WithIgnoreMutationWebhook(false))
	}
	return &differ{serverSide: true, serverOpts: opts, stripLabel: stripLabel, log: e.log}, cleanup, nil
}

// noiseNormalizer strips the same server-managed and ksync-bookkeeping fields
// normalizeForDiff removes, but in place — gitops-engine's server-side diff
// applies the normalizer to the live and predicted-live objects (its default is
// a no-op, which would surface status and managedFields as diffs). The rendered
// target carries none of these, so stripping both the predicted and live sides
// keeps the diff to the author's change. stripLabel follows the same
// display-vs-apply-decision contract as stripDiffNoise.
type noiseNormalizer struct{ stripLabel bool }

func (n noiseNormalizer) Normalize(un *unstructured.Unstructured) error {
	if un != nil {
		stripDiffNoise(un, n.stripLabel)
	}
	return nil
}
