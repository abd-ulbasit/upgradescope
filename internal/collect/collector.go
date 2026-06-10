// Package collect builds an inventory.Inventory from a live cluster (or,
// in files mode, from rendered manifests). Sub-collectors degrade
// independently: an error marks the capability unavailable with a reason
// instead of failing the whole collection (spec §9).
package collect

import (
	"context"
	"errors"
	"fmt"
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

// listPageSize bounds every cluster-wide list call: large clusters must
// never be read in one unbounded request.
const listPageSize = 500

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

// partialError marks a step that produced usable data but not all of it
// (e.g. one forbidden resource among many). runSteps keeps the capability
// available and surfaces the message as the Reason.
type partialError struct{ msg string }

func (e partialError) Error() string { return e.msg }

func runSteps(ctx context.Context, inv *inventory.Inventory, ss []step) {
	for _, s := range ss {
		err := s.run(ctx, inv)
		var pe partialError
		switch {
		case err == nil:
			inv.Capabilities[s.cap] = inventory.CapabilityStatus{Available: true}
		case errors.As(err, &pe):
			inv.Capabilities[s.cap] = inventory.CapabilityStatus{Available: true, Reason: pe.Error()}
		default:
			inv.Capabilities[s.cap] = inventory.CapabilityStatus{Available: false, Reason: err.Error()}
		}
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
		{cap: inventory.CapAddOns, run: func(ctx context.Context, inv *inventory.Inventory) error { // after helm: consumes inv.HelmReleases
			if c.Kube == nil {
				return errors.New("kubernetes client not configured")
			}
			return collectAddOns(ctx, c.Kube, k.AddOns, inv)
		}},
		{cap: inventory.CapAPIUsage, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.Discovery == nil || c.Metadata == nil {
				return errors.New("discovery/metadata client not configured")
			}
			return collectAPIUsage(ctx, c.Discovery, c.Metadata, k.APILifecycle, inv)
		}},
	}
}

// NewClients builds the concrete client set from a rest.Config.
// The sole construction point — everything else consumes the interfaces.
func NewClients(cfg *rest.Config) (Clients, error) {
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("build kubernetes client: %w", err)
	}
	md, err := metadata.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("build metadata client: %w", err)
	}
	return Clients{
		Kube:       kube,
		Metadata:   md,
		Discovery:  kube.Discovery(),
		RESTClient: kube.CoreV1().RESTClient(),
	}, nil
}
