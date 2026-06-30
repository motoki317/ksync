package render

import (
	"errors"
	"strings"
	"testing"
)

// kustomize wraps a failed `helm template` exec as
//
//	<helm stderr>: unable to run: '<helm> <args>' with env=[...] (is '<helm>' installed?): exit status N
//
// (sigs.k8s.io/kustomize/.../HelmChartInflationGenerator.go runHelmCommand). The
// command dump, env, and the "(is X installed?)" tail are pure noise — the helm
// binary is ksync's own temp wrapper, always present — so cleanRenderError strips
// them and, for a missing-CRD failure, replaces the message with the two real
// fixes. The fixtures below use invented identifiers so no live cluster name leaks.

// A live render against a cluster lacking a chart's CRD fails server-side mapping;
// cleanRenderError names the kind and points at the fixes — clientRender for a
// chart that bundles its own CRD, installing the CRD when it belongs to another
// app, or an offline render — dropping every byte of kustomize's exec noise.
func TestCleanRenderError_MissingCRD(t *testing.T) {
	raw := errors.New(`Error: unable to build kubernetes objects from release manifest: resource mapping not found for name: "shop-widget" namespace: "shop" from "": no matches for kind "Widget" in version "example.com/v1alpha1"
ensure CRDs are installed first: unable to run: '/tmp/ksync-helm-123/helm template shop /repo/charts/local-shop --namespace shop -f /tmp/kustomize-helm-456/local-shop-kustomize-values.yaml --include-crds' with env=[HELM_CONFIG_HOME=/tmp/kustomize-helm-456/helm HELM_CACHE_HOME=/tmp/kustomize-helm-456/helm/.cache HELM_DATA_HOME=/tmp/kustomize-helm-456/helm/.data] (is '/tmp/ksync-helm-123/helm' installed?): exit status 1`)

	got := cleanRenderError(raw)
	msg := got.Error()

	// The bundled-CRD case is the symptom this feature targets, so the error must
	// name clientRender — the durable per-app fix — alongside the offline escape.
	for _, want := range []string{"Widget", "example.com/v1alpha1", "clientRender", "--offline-render"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	// Also names installing the CRD (when it belongs to another app), conditionally
	// — the same helm error covers an unserved built-in apiVersion, so it must not
	// flatly assert "no CRD".
	if !strings.Contains(strings.ToLower(msg), "install") {
		t.Errorf("message should point at installing the CRD:\n%s", msg)
	}
	for _, noise := range []string{"unable to run", "env=[", "installed?", "exit status", "/tmp/ksync-helm", "kustomize-helm"} {
		if strings.Contains(msg, noise) {
			t.Errorf("message still carries kustomize noise %q:\n%s", noise, msg)
		}
	}
	if !errors.Is(got, raw) {
		t.Errorf("raw error must stay reachable via Unwrap for -v / errors.Is")
	}
}

// A helm exec failure that is not a CRD-mapping error (e.g. a template bug) still
// gets the kustomize noise stripped, surfacing helm's own message verbatim — never
// an empty or half-built one. This is the fail-safe degrade when the specific
// pattern does not match (a helm wording change must not break the cleanup).
func TestCleanRenderError_NonCRDHelmFailure(t *testing.T) {
	raw := errors.New(`Error: template: local-shop/templates/deploy.yaml:10:5: executing "local-shop/templates/deploy.yaml" at <.Values.missing>: nil pointer evaluating interface {}.field: unable to run: '/tmp/ksync-helm-123/helm template shop /repo/charts/local-shop --include-crds' with env=[HELM_CONFIG_HOME=/tmp/x HELM_CACHE_HOME=/tmp/x/.cache HELM_DATA_HOME=/tmp/x/.data] (is '/tmp/ksync-helm-123/helm' installed?): exit status 1`)

	msg := cleanRenderError(raw).Error()

	if !strings.Contains(msg, "nil pointer evaluating") {
		t.Errorf("helm's own template error must survive:\n%s", msg)
	}
	for _, noise := range []string{"unable to run", "env=[", "installed?", "exit status"} {
		if strings.Contains(msg, noise) {
			t.Errorf("kustomize noise %q not stripped:\n%s", noise, msg)
		}
	}
	if strings.TrimSpace(msg) == "" {
		t.Fatal("cleaned message must never be empty")
	}
}

// An error that is not a helm exec failure is returned untouched, so unrelated
// render failures keep their original wording and identity.
func TestCleanRenderError_PassthroughUnrelated(t *testing.T) {
	raw := errors.New("accumulating resources: open kustomization.yaml: no such file or directory")
	if got := cleanRenderError(raw); got != raw {
		t.Errorf("unrelated error must be returned unchanged; got %v", got)
	}
}

// The exec signature with no helm stderr before it (helm wrote nothing, or only
// whitespace) leaves nothing to surface — so the raw error is kept rather than a
// blank cleaned message.
func TestCleanRenderError_EmptyStderrKeepsRaw(t *testing.T) {
	raw := errors.New(`   : unable to run: '/tmp/ksync-helm-123/helm template shop /repo/charts/local-shop' with env=[HELM_CONFIG_HOME=/tmp/x] (is '/tmp/ksync-helm-123/helm' installed?): exit status 1`)
	if got := cleanRenderError(raw); got != raw {
		t.Errorf("empty helm stderr must keep the raw error; got %v", got)
	}
}
