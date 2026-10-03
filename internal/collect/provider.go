package collect

import (
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// providerRule is what identifies one managed Kubernetes service. A service
// is claimed by its control plane's server-version suffix or by a label its
// managed node pools put on nodes; either alone is enough. schemes are the
// providerID schemes its nodes carry: a node with another scheme contradicts
// the claim (GKE's version suffix on vSphere nodes is GKE on-prem, which is
// not billed as GKE). A providerID scheme is never a claim of its own,
// because the same scheme names a VM's cloud for any self-managed cluster
// on it.
type providerRule struct {
	provider inventory.Provider
	version  *regexp.Regexp // matched against the raw server GitVersion; nil: the service adds no suffix
	labels   []string       // node label keys
	schemes  []string       // providerID schemes of the service's nodes
}

var providerRules = []providerRule{
	{
		provider: inventory.ProviderEKS,
		// The build is a commit hash ("v1.34.2-eks-3abc123"); EKS Distro and
		// EKS Anywhere use "-eks-1-29-12", which is not the managed service.
		version: regexp.MustCompile(`^v\d+\.\d+\.\d+-eks-[0-9a-f]{6,}$`),
		labels:  []string{"eks.amazonaws.com/nodegroup", "eks.amazonaws.com/compute-type"},
		schemes: []string{"aws", "eks-hybrid"}, // Fargate nodes are aws://; hybrid nodes eks-hybrid://
	},
	{
		provider: inventory.ProviderGKE,
		version:  regexp.MustCompile(`^v\d+\.\d+\.\d+-gke\.\d+$`),
		labels:   []string{"cloud.google.com/gke-nodepool"},
		schemes:  []string{"gce"},
	},
	{
		provider: inventory.ProviderAKS,
		// AKS puts nothing in the version; its nodes carry the cluster's
		// node resource group.
		labels:  []string{"kubernetes.azure.com/cluster"},
		schemes: []string{"azure"},
	},
}

// providerEvidence is what the collector read that bears on the provider:
// the labels of the nodes' managed node pools and their providerID schemes,
// without keeping the nodes.
type providerEvidence struct {
	labelled  map[inventory.Provider]bool // a node carries this service's label
	schemes   map[string]bool             // providerID schemes seen
	nodesRead bool                        // the node list completed
}

func (e *providerEvidence) addNode(n *corev1.Node) {
	for _, r := range providerRules {
		if slices.ContainsFunc(r.labels, func(k string) bool { _, ok := n.Labels[k]; return ok }) {
			if e.labelled == nil {
				e.labelled = map[inventory.Provider]bool{}
			}
			e.labelled[r.provider] = true
		}
	}
	if scheme, _, ok := strings.Cut(n.Spec.ProviderID, "://"); ok && scheme != "" {
		if e.schemes == nil {
			e.schemes = map[string]bool{}
		}
		e.schemes[scheme] = true
	}
}

// provider infers the managed service from the server version and the
// nodes read. Exactly one service must claim the cluster and none of the
// nodes' providerID schemes may contradict it; two claims, or a
// contradiction, are other. A cluster nothing claims is other too, but only
// when its nodes were read: without them an AKS cluster, which has no
// version suffix, cannot be told from a vanilla one, so it is left
// undetermined (""). When the node list failed partway the pages read are
// not evidence either: a label on one page could claim a service that a
// providerID on a page not read would contradict, so only the version
// suffix is used then.
func (e providerEvidence) provider(serverVersion string) inventory.Provider {
	if !e.nodesRead {
		e = providerEvidence{}
	}
	var claimed []providerRule
	for _, r := range providerRules {
		if r.version != nil && r.version.MatchString(serverVersion) || e.labelled[r.provider] {
			claimed = append(claimed, r)
		}
	}
	switch {
	case len(claimed) > 1:
		return inventory.ProviderOther
	case len(claimed) == 0:
		if !e.nodesRead {
			return ""
		}
		return inventory.ProviderOther
	}
	for scheme := range e.schemes {
		if !slices.Contains(claimed[0].schemes, scheme) {
			return inventory.ProviderOther
		}
	}
	return claimed[0].provider
}
