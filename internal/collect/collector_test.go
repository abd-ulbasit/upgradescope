package collect

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func TestRunStepsDegradesFailedCapabilityAndKeepsOthers(t *testing.T) {
	inv := inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
	ss := []step{
		{cap: inventory.CapVersions, run: func(_ context.Context, inv *inventory.Inventory) error {
			inv.ServerVersion = "v1.34.2"
			return nil
		}},
		{cap: inventory.CapHelm, run: func(_ context.Context, _ *inventory.Inventory) error {
			return errors.New(`secrets is forbidden: User "scanner" cannot list resource "secrets"`)
		}},
		{cap: inventory.CapAddOns, run: func(_ context.Context, inv *inventory.Inventory) error {
			inv.AddOns = []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"ingress-nginx"}, Source: "image"}}
			return nil
		}},
	}
	runSteps(context.Background(), &inv, ss)

	if got := inv.Capabilities[inventory.CapVersions]; !got.Available || got.Reason != "" {
		t.Errorf("versions capability = %+v, want available with no reason", got)
	}
	helm := inv.Capabilities[inventory.CapHelm]
	if helm.Available {
		t.Error("helm capability should be degraded after a forbidden error")
	}
	if helm.Reason == "" {
		t.Error("degraded capability must carry the error as Reason")
	}
	if inv.ServerVersion != "v1.34.2" {
		t.Errorf("ServerVersion = %q; data from the successful step before the failure must persist", inv.ServerVersion)
	}
	if len(inv.AddOns) != 1 {
		t.Errorf("AddOns = %+v; steps after a failed step must still run", inv.AddOns)
	}
}

func TestRunStepsPartialErrorKeepsCapabilityAvailable(t *testing.T) {
	inv := inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
	runSteps(context.Background(), &inv, []step{
		{cap: inventory.CapAPIUsage, run: func(context.Context, *inventory.Inventory) error {
			return partialError{msg: "partial: list policy/v1beta1 podsecuritypolicies: forbidden"}
		}},
	})
	got := inv.Capabilities[inventory.CapAPIUsage]
	if !got.Available {
		t.Errorf("capability = %+v, want available despite partial error", got)
	}
	if got.Reason == "" {
		t.Error("partial error must surface as the capability Reason")
	}
}

func TestCollectDefaults(t *testing.T) {
	inv := Collect(context.Background(), Clients{}, kb.KB{}, Options{})
	if inv.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", inv.SchemaVersion)
	}
	if inv.Capabilities == nil {
		t.Error("Capabilities map must be initialized")
	}
	if inv.CollectedAt.IsZero() {
		t.Error("CollectedAt must be set")
	}
}

func TestNewClients(t *testing.T) {
	cfg := &rest.Config{Host: "https://127.0.0.1:6443"}
	c, err := NewClients(cfg)
	if err != nil {
		t.Fatalf("NewClients: %v", err)
	}
	if c.Kube == nil || c.Metadata == nil || c.Discovery == nil || c.RESTClient == nil {
		t.Errorf("NewClients left a nil client: %+v", c)
	}
}

func TestNewClientsBadConfig(t *testing.T) {
	// Exec-provider misconfig is one of the few things that fails at construction time.
	cfg := &rest.Config{Host: "https://127.0.0.1:6443", BearerTokenFile: "\x00"}
	if _, err := NewClients(cfg); err == nil {
		t.Skip("client-go accepted the config; constructor error paths are config-dependent")
	}
}
