package registry

import "strings"

// ProviderBuildPrefixes are where managed Kubernetes providers publish their
// own builds of open-source add-ons: GKE (network policy, Dataplane V2) and
// AKS (Calico, Azure CNI powered by Cilium, the Istio, KEDA and application
// routing add-ons). Those builds follow the provider's support policy, not
// upstream's, so judging them by an upstream entry raises false EOL
// blockers (#18). Host-less matchers therefore never claim them; an entry
// for a provider build names its location in a host-qualified matcher
// ("mcr.microsoft.com/oss/kubernetes/ingress/nginx-ingress-controller").
var ProviderBuildPrefixes = []string{
	"gke.gcr.io/",
	"gcr.io/gke-release/",
	"mcr.microsoft.com/",
}

// IsProviderBuild reports whether a normalised "host/path" repository, or a
// matcher, lies under a provider prefix, directly or through a mirror that
// keeps the provider location in its path
// ("harbor.example/mcr.microsoft.com/oss/calico/node").
func IsProviderBuild(repo string) bool {
	for _, p := range ProviderBuildPrefixes {
		if strings.HasPrefix(repo, p) || strings.Contains(repo, "/"+p) {
			return true
		}
	}
	return false
}
