package engine

import (
	"fmt"
	"sort"

	"k8s.io/client-go/discovery"
)

// DiscoverCapabilities reads the named context's cluster API versions and
// Kubernetes version in the forms helm's `--api-versions` / `--kube-version`
// flags expect. ksync renders charts against the live cluster, so feeding helm
// the cluster's real capabilities is what makes version-gated templates resolve
// correctly — e.g. a PodDisruptionBudget guarded by
// `.Capabilities.APIVersions.Has "policy/v1/PodDisruptionBudget"`. Helm v4 fills
// Capabilities from `--dry-run=server`, but helm v3 does NOT: under v3 a chart
// silently falls back to a removed apiVersion (policy/v1beta1) that then fails to
// apply. Passing the discovered set makes both helm versions render for the
// actual target cluster.
//
// Both membership forms are emitted for every resource — "group/version" and
// "group/version/Kind" — because charts test either.
func DiscoverCapabilities(kubeContext string) (apiVersions []string, kubeVersion string, err error) {
	cfg, err := RESTConfig(kubeContext)
	if err != nil {
		return nil, "", err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, "", err
	}

	_, lists, err := dc.ServerGroupsAndResources()
	// Partial discovery (a flaky aggregated API server) returns the groups it
	// could reach alongside the error; those are still usable, so only a total
	// failure is fatal.
	if err != nil && !discovery.IsGroupDiscoveryFailedError(err) {
		return nil, "", fmt.Errorf("discovering cluster API versions: %w", err)
	}

	seen := make(map[string]struct{})
	add := func(s string) {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			apiVersions = append(apiVersions, s)
		}
	}
	for _, list := range lists {
		if list == nil || list.GroupVersion == "" {
			continue
		}
		add(list.GroupVersion)
		for _, r := range list.APIResources {
			if r.Kind != "" {
				add(list.GroupVersion + "/" + r.Kind)
			}
		}
	}
	sort.Strings(apiVersions)

	ver, err := dc.ServerVersion()
	if err != nil {
		return nil, "", fmt.Errorf("discovering cluster Kubernetes version: %w", err)
	}
	return apiVersions, ver.GitVersion, nil
}
