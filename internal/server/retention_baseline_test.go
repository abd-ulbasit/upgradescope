package server

import (
	"context"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// widgetKB is testKB plus a second API removed in 1.35, so a later push
// can add a blocker the baseline does not have.
func widgetKB() kb.KB {
	k := testKB()
	removed := inventory.Version{Major: 1, Minor: 35}
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Group: "example.dev", Version: "v1alpha1", Kind: "Widget",
		Introduced: inventory.Version{Major: 1, Minor: 10}, Removed: &removed,
	})
	return k
}

func withWidget(inv inventory.Inventory) inventory.Inventory {
	inv.APIUsage = append(append([]inventory.APIUsage(nil), inv.APIUsage...), inventory.APIUsage{
		Group: "example.dev", Version: "v1alpha1", Kind: "Widget", Count: 1, Namespaces: map[string]int{"apps": 1},
	})
	return inv
}

// TestRetentionKeepsTheNotificationBaseline (#241): a cluster blocked by
// PSP, then unknown (api-usage unavailable) for longer than --retention,
// then decided again. Pruning used to delete the last decided evaluation,
// so the next decided pass had no baseline and announced nothing: neither
// the new blocker nor, in the other ending, became-ready.
func TestRetentionKeepsTheNotificationBaseline(t *testing.T) {
	for _, c := range []struct {
		name string
		last inventory.Inventory
		want string
	}{
		{"new blocker", withWidget(testInventoryWithPSP()), notify.KindNewBlocker},
		{"became ready", testInventory(), notify.KindBecameReady},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, Config{KB: widgetKB(), Retention: 90 * 24 * time.Hour}, aug1)
			h.push("prod", testInventoryWithPSP()) // blocked: the baseline
			h.clock.set(aug1.Add(24 * time.Hour))
			unknown := testInventory()
			unknown.Capabilities = collectedCaps()
			unknown.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Reason: "forbidden"}
			h.push("prod", unknown) // unknown: no baseline, and a new snapshot
			if evs := h.drain(); len(evs) != 0 {
				t.Fatalf("events before the stretch = %+v, want none", evs)
			}
			h.clock.set(aug1.Add(101 * 24 * time.Hour))
			h.srv.pruneOnce(context.Background())
			h.push("prod", c.last)
			evs := h.drain()
			if len(evs) != 1 || evs[0].Kind != c.want {
				t.Errorf("events after pruning = %+v, want one %s", evs, c.want)
			}
		})
	}
}
