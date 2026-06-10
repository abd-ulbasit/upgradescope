// Package collect builds an inventory.Inventory from a live cluster (or,
// in files mode, from rendered manifests). Sub-collectors degrade
// independently: an error marks the capability unavailable with a reason
// instead of failing the whole collection (spec §9).
package collect

import (
	"context"
	"errors"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// Clients bundles the API clients the live-cluster sub-collectors need.
// Any nil client degrades the capabilities that depend on it.
type Clients struct {
	Kube       kubernetes.Interface
	Metadata   metadata.Interface
	Discovery  discovery.DiscoveryInterface
	RESTClient rest.Interface
}

// Options tunes collection behavior.
type Options struct {
	TeamLabel string // namespace label used for team attribution; default "team"
}

// step is one independently-degradable sub-collector bound to a capability.
type step struct {
	cap inventory.Capability
	run func(ctx context.Context, inv *inventory.Inventory) error
}

// Collect builds an Inventory from a live cluster. It never returns an
// error: each sub-collector failure becomes Capabilities[cap] =
// {Available: false, Reason: err.Error()} and collection continues.
func Collect(ctx context.Context, c Clients, k kb.KB, opts Options) inventory.Inventory {
	if opts.TeamLabel == "" {
		opts.TeamLabel = "team"
	}
	inv := inventory.Inventory{
		SchemaVersion: 1,
		CollectedAt:   time.Now().UTC(),
		Capabilities:  map[inventory.Capability]inventory.CapabilityStatus{},
	}
	runSteps(ctx, &inv, steps(c, k, opts))
	return inv
}

func runSteps(ctx context.Context, inv *inventory.Inventory, ss []step) {
	for _, s := range ss {
		if err := s.run(ctx, inv); err != nil {
			inv.Capabilities[s.cap] = inventory.CapabilityStatus{Available: false, Reason: err.Error()}
			continue
		}
		inv.Capabilities[s.cap] = inventory.CapabilityStatus{Available: true}
	}
}

// steps lists the live sub-collectors in execution order (helm must run
// before addons: the add-on matcher consumes inv.HelmReleases).
// Tasks C2–C6 append one entry each as the sub-collectors land.
func steps(c Clients, k kb.KB, opts Options) []step {
	return []step{
		{cap: inventory.CapVersions, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.Kube == nil || c.Discovery == nil {
				return errors.New("kubernetes client not configured")
			}
			return collectVersions(ctx, c.Discovery, c.Kube, opts.TeamLabel, inv)
		}},
		{cap: inventory.CapHelm, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.Kube == nil {
				return errors.New("kubernetes client not configured")
			}
			return collectHelm(ctx, c.Kube, inv)
		}},
		{cap: inventory.CapDeprecatedCalls, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.RESTClient == nil {
				return errors.New("rest client not configured")
			}
			return collectDeprecatedCalls(ctx, c.RESTClient, inv)
		}},
	}
}
