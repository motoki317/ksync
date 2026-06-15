package engine

import (
	"errors"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// RESTConfig builds a client config for exactly the named kubectl context.
// There is deliberately no empty-means-current-context convenience here: the
// caller resolves the context (current-context or --context) and validates it
// against ksync.yaml's allowedContexts before reaching this point, so an empty
// name is a programming error, not a fall-through to whatever happens to be
// current.
func RESTConfig(kubeContext string) (*rest.Config, error) {
	if kubeContext == "" {
		return nil, errors.New("kube context must be explicitly named (ksync never uses the current-context)")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	// client-go's default rate limit (QPS 5, burst 10) throttles ksync hard:
	// measured against a local cluster, cache warm-up spent >10s waiting on
	// discovery requests and the engine's concurrent applies were serialized
	// to ~55ms each. A long-running local-dev tool talking to its own local
	// cluster can be generous; these match ArgoCD's kube-client defaults.
	cfg.QPS = 50
	cfg.Burst = 100
	return cfg, nil
}

// CurrentContext returns the kubeconfig's current-context (empty if none is
// set). ksync uses it as the default target, then enforces ksync.yaml's
// allowedContexts — so reading current-context here is a convenience for
// selection, never a license to act on it unchecked.
func CurrentContext() (string, error) {
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).RawConfig()
	if err != nil {
		return "", err
	}
	return raw.CurrentContext, nil
}
