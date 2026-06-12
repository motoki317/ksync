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
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).ClientConfig()
}
