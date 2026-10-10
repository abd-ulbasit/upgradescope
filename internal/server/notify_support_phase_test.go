package server

import (
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// #266: the extended-to-ended transition is news. The key names the phase,
// so the out-of-support blocker is not the extended-support blocker the
// notifier already sent; ComputeDelta across the boundary (a fixed clock
// either side of extended_end, EKS 1.34: 2027-12-02) is one new-blocker.
func TestComputeDeltaAtTheEndOfExtendedSupport(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.Inventory{SchemaVersion: 1, ClusterID: "c", Source: inventory.SourceCluster, Provider: inventory.ProviderEKS, ServerVersion: "v1.34.2-eks-3abc123"}
	at := func(day string) engine.Report {
		d, err := time.Parse("2006-01-02", day)
		if err != nil {
			t.Fatal(err)
		}
		return engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 35}, d)
	}
	before := at("2027-12-01")
	changes := ComputeDelta(&before, at("2027-12-02"))
	if len(changes) != 1 || changes[0].Kind != notify.KindNewBlocker || !strings.Contains(changes[0].Title, "out of Amazon EKS support") {
		t.Fatalf("changes across extended_end = %+v, want one new-blocker for the out-of-support finding", changes)
	}
	// Standard support ending is news too: the warning becomes a blocker.
	ending := at("2026-12-01")
	if changes := ComputeDelta(&ending, at("2026-12-02")); len(changes) != 1 || changes[0].Kind != notify.KindNewBlocker {
		t.Errorf("changes across standard_end = %+v, want one new-blocker", changes)
	}
	// Within a phase nothing changes.
	again := at("2027-06-01")
	if changes := ComputeDelta(&again, at("2027-06-02")); len(changes) != 0 {
		t.Errorf("changes within extended support = %+v, want none", changes)
	}
}
