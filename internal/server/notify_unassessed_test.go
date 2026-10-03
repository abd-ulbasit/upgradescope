package server

import (
	"context"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// A capability a pass did not assess hides its findings without anything
// being fixed (#189). These tests push through the real ingest on SQLite
// (testKB: PSP removed in 1.35; the cluster is on v1.34.2, target 1.35),
// as TestUnknownVerdictNeverReadyOrAlerting does.

// withServiceCIDRCaller adds a deprecated-calls row for v1beta1
// ServiceCIDR removed in 1.35: a blocker at the default target, from the
// /metrics scrape alone (no API usage to fold into).
func withServiceCIDRCaller(inv inventory.Inventory) inventory.Inventory {
	inv.Capabilities[inventory.CapDeprecatedCalls] = inventory.CapabilityStatus{Available: true}
	inv.DeprecatedCalls = []inventory.DeprecatedCall{{Group: "networking.k8s.io", Version: "v1beta1", Resource: "servicecidrs", RemovedRelease: "1.35"}}
	return inv
}

// callsScraped is inv with a /metrics scrape that recorded no deprecated
// call.
func callsScraped(inv inventory.Inventory) inventory.Inventory {
	inv.Capabilities[inventory.CapDeprecatedCalls] = inventory.CapabilityStatus{Available: true}
	return inv
}

// callsUnavailable is inv with the /metrics scrape failed.
func callsUnavailable(inv inventory.Inventory) inventory.Inventory {
	inv.Capabilities[inventory.CapDeprecatedCalls] = inventory.CapabilityStatus{Available: false, Reason: "get /metrics: context deadline exceeded"}
	inv.DeprecatedCalls = nil
	return inv
}

func expectNoEvents(t *testing.T, h *harness, step string) {
	t.Helper()
	if evs := h.drain(); len(evs) != 0 {
		t.Fatalf("%s notified: %+v", step, evs)
	}
}

func expectBecameReady(t *testing.T, h *harness, step string) {
	t.Helper()
	evs := h.drain()
	if len(evs) != 1 || evs[0].Kind != notify.KindBecameReady {
		t.Fatalf("%s: events = %+v, want exactly one became-ready", step, evs)
	}
}

// TestUnassessedCapabilityKeepsItsBlockers: blocked{A,B} → blocked{A}
// with B's capability unavailable → blocked{A,B} sends nothing, and the
// genuine resolution, assessed and gone, still sends became-ready.
func TestUnassessedCapabilityKeepsItsBlockers(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("prod", withServiceCIDRCaller(testInventoryWithPSP())) // baseline: PSP + caller
	expectNoEvents(t, h, "first evaluation")

	h.push("prod", callsUnavailable(testInventoryWithPSP())) // still blocked by PSP
	expectNoEvents(t, h, "/metrics failure")
	h.push("prod", withServiceCIDRCaller(testInventoryWithPSP()))
	expectNoEvents(t, h, "/metrics back, caller unchanged")

	h.push("prod", callsScraped(testInventoryWithPSP())) // caller really gone, PSP left
	expectNoEvents(t, h, "caller resolved while PSP remains")
	h.push("prod", callsScraped(testInventory()))
	expectBecameReady(t, h, "PSP resolved")
}

// TestUnassessedCapabilityNeverBecomesReady: blocked{deprecated call
// only} → deprecated-calls unavailable is "ready" (the capability is
// optional), but sends no became-ready; the capability's return with the
// caller still there is not news, across several passes and a
// re-evaluation of the outage.
func TestUnassessedCapabilityNeverBecomesReady(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("prod", withServiceCIDRCaller(testInventory()))
	expectNoEvents(t, h, "first evaluation")

	h.push("prod", callsUnavailable(testInventory()))
	if c := h.fleetCell("prod", "1.35"); c == nil || c.Verdict != "ready" {
		t.Fatalf("cell with deprecated-calls unavailable = %+v, want the engine's verdict, ready", c)
	}
	expectNoEvents(t, h, "/metrics failure")
	// The stored report carries the caller forward; the served one is
	// the engine's report.
	cur, err := h.st.CurrentEvaluation(context.Background(), h.clusterID("prod"), "1.35")
	if err != nil {
		t.Fatal(err)
	}
	if heads, err := storedFindingHeads(cur.Report); err != nil || len(heads.CarriedForward) != 1 {
		t.Fatalf("stored heads = %+v, %v; want the caller carried forward", heads, err)
	}
	var served map[string]any
	h.get("/api/v1/clusters/"+itoa(h.clusterID("prod"))+"/report?target=1.35", &served)
	if _, ok := served["carriedForward"]; ok || served["verdict"] != "ready" {
		t.Fatalf("served report = %v, want the engine's report (ready, no carriedForward)", served)
	}

	// The outage outlives a pass with other changes and a re-evaluation
	// of the stored snapshot (a new UTC day).
	other := callsUnavailable(testInventory())
	other.Namespaces = []inventory.NamespaceInfo{{Name: "shop", Team: "payments"}}
	h.push("prod", other)
	expectNoEvents(t, h, "second pass of the outage")
	h.clock.set(aug1.Add(24 * time.Hour))
	h.tick()
	expectNoEvents(t, h, "re-evaluation of the outage")

	h.push("prod", withServiceCIDRCaller(testInventory()))
	expectNoEvents(t, h, "/metrics back, caller unchanged")

	h.push("prod", callsScraped(testInventory()))
	expectBecameReady(t, h, "caller resolved, assessed")
	h.tick()
	h.push("prod", callsScraped(testInventory()))
	expectNoEvents(t, h, "after became-ready")
}

// TestRequiredCapabilityOutageKeepsItsBlockers: blocked{PSP, caller}
// with api-usage unavailable stays decided (the caller blocks), and
// api-usage's recovery does not announce PSP again.
func TestRequiredCapabilityOutageKeepsItsBlockers(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("prod", withServiceCIDRCaller(testInventoryWithPSP()))
	expectNoEvents(t, h, "first evaluation")

	broken := withServiceCIDRCaller(testInventory())
	broken.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: false, Reason: "forbidden"}
	h.push("prod", broken)
	if c := h.fleetCell("prod", "1.35"); c == nil || c.Verdict != "blocked" {
		t.Fatalf("cell with api-usage unavailable = %+v, want blocked by the caller", c)
	}
	expectNoEvents(t, h, "api-usage failure")

	h.push("prod", withServiceCIDRCaller(testInventoryWithPSP()))
	expectNoEvents(t, h, "api-usage back, PSP unchanged")
}

// TestUnassessedCapabilityStillAnnouncesNewBlockers: a capability gap
// holds only the blockers it hides. A blocker another capability finds
// during the outage is news.
func TestUnassessedCapabilityStillAnnouncesNewBlockers(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("prod", withServiceCIDRCaller(testInventory()))
	h.drain()

	h.push("prod", callsUnavailable(testInventoryWithPSP()))
	evs := h.drain()
	if len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker {
		t.Fatalf("events = %+v, want one new-blocker for PSP", evs)
	}
}
