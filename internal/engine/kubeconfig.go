package engine

import (
	"errors"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// RESTConfig builds a client config for exactly the named kubectl context.
// There is deliberately no empty-means-current-context convenience: the
// explicit name in ksync.yaml is the whole context-safety model.
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
