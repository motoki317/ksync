package render

import (
	"fmt"
	"regexp"
	"strings"
)

// helmRenderError carries a cleaned, actionable message for a kustomize/helm
// render failure while keeping the raw error reachable (Unwrap) for -v and
// errors.Is/As.
type helmRenderError struct {
	msg string
	raw error
}

func (e *helmRenderError) Error() string { return e.msg }
func (e *helmRenderError) Unwrap() error { return e.raw }

// helmExecNoise matches the suffix kustomize appends to a failed `helm template`
// exec (HelmChartInflationGenerator.runHelmCommand): the command line, the env
// dump, and a "(is '<helm>' installed?)" tail that is actively misleading — the
// helm binary is ksync's own temp wrapper, always present. Stripping it surfaces
// helm's own stderr, which precedes it.
var helmExecNoise = regexp.MustCompile(`: unable to run: '.*' with env=\[.*\] \(is '.*' installed\?\): exit status \d+`)

// missingCRD matches helm's report of a kind the cluster cannot map — what a live
// render (`helm template --dry-run=server`) emits when the chart renders a custom
// resource whose CRD is not yet installed.
var missingCRD = regexp.MustCompile(`no matches for kind "([^"]+)" in version "([^"]+)"`)

// cleanRenderError turns a kustomize helm-exec failure into a concise, actionable
// error: the kustomize command/env/"installed?" noise is stripped, and a missing
// CRD (the common failure of a live render against a fresh cluster) is reported as
// the kind plus the two real fixes. Any error without the helm-exec signature is
// returned unchanged; the helm-exec signature without a recognized inner pattern
// degrades to helm's own stderr — never an empty or half-built message.
func cleanRenderError(err error) error {
	s := err.Error()
	loc := helmExecNoise.FindStringIndex(s)
	if loc == nil {
		return err
	}
	helmStderr := strings.TrimSpace(s[:loc[0]])
	if helmStderr == "" {
		// Nothing but exec noise to begin with: keep the raw error rather than
		// hand back an empty message.
		return err
	}
	return &helmRenderError{msg: actionableRenderMessage(helmStderr), raw: err}
}

func actionableRenderMessage(helmStderr string) string {
	// An unmappable kind is usually a custom resource whose CRD is missing, but the
	// same helm error covers a built-in apiVersion the cluster does not serve — so
	// the fix is phrased conditionally rather than asserting "no CRD".
	if m := missingCRD.FindStringSubmatch(helmStderr); m != nil {
		return fmt.Sprintf(
			"cluster cannot map %s (%s).\n"+
				"fix: if it is a custom resource, install its CRD first (apply the base layer); or render offline: --offline-render (helm `lookup` results will be empty)",
			m[1], m[2])
	}
	return helmStderr
}
