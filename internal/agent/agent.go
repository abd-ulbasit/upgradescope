// Package agent runs the in-cluster continuous loop: collect → evaluate per
// target → ClusterReadiness CRD status (always) → push snapshot to the server
// on content change. The agent's local value never depends on server
// availability (spec §3).
package agent

import (
	"fmt"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// AgentVersion is stamped into CRD status and push envelopes. The CLI sets it
// from the build version; "dev" otherwise.
var AgentVersion = "dev"

type Config struct {
	Interval       time.Duration // default 10m, min 1m
	ServerURL      string        // optional; "" = CRD-only mode
	ServerToken    string        // bearer for push
	ClusterName    string        // human label sent to server; default = ClusterID
	CRName         string        // default crd.DefaultName
	TeamLabel      string        // default "team"
	ForceSyncEvery time.Duration // default 1h: push even if hash unchanged
}

// applyDefaults fills zero values and rejects invalid combinations.
func (c *Config) applyDefaults() error {
	if c.Interval == 0 {
		c.Interval = 10 * time.Minute
	}
	if c.Interval < time.Minute {
		return fmt.Errorf("interval %s below minimum 1m", c.Interval)
	}
	if c.CRName == "" {
		c.CRName = crd.DefaultName
	}
	if c.TeamLabel == "" {
		c.TeamLabel = "team"
	}
	if c.ForceSyncEvery == 0 {
		c.ForceSyncEvery = time.Hour
	}
	if c.ServerURL != "" && c.ServerToken == "" {
		return fmt.Errorf("server-url set but server-token empty (the ingest endpoint requires a bearer token)")
	}
	return nil
}

// resolveTargets picks evaluation targets per tick: spec.Targets if any parse
// (invalid entries are skipped with a note for status.notAssessed), else the
// next minor above the observed server version. Resolved per-tick because the
// spec can change at any time.
func resolveTargets(spec crd.Spec, inv inventory.Inventory) (targets []inventory.Version, notes []string, err error) {
	for _, raw := range spec.Targets {
		v, perr := inventory.ParseVersion(raw)
		if perr != nil {
			notes = append(notes, fmt.Sprintf("targets: skipped invalid spec target %q", raw))
			continue
		}
		targets = append(targets, v)
	}
	if len(targets) > 0 {
		return targets, notes, nil
	}
	if inv.ServerVersion == "" {
		return nil, notes, fmt.Errorf("targets: no spec targets and server version unknown")
	}
	observed, perr := inventory.ParseVersion(inv.ServerVersion)
	if perr != nil {
		return nil, notes, fmt.Errorf("targets: no spec targets and server version %q unparseable: set spec.targets explicitly", inv.ServerVersion)
	}
	return []inventory.Version{observed.Next()}, notes, nil
}
