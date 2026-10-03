package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
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

// withPSPCaller adds a deprecated-calls row for the PSP objects of
// testInventoryWithPSP: with them, the engine folds it into the PSP
// finding, which keeps its key; without them, it is a finding of its own.
func withPSPCaller(inv inventory.Inventory) inventory.Inventory {
	inv.Capabilities[inventory.CapDeprecatedCalls] = inventory.CapabilityStatus{Available: true}
	inv.DeprecatedCalls = []inventory.DeprecatedCall{{Group: "policy", Version: "v1beta1", Resource: "podsecuritypolicies", RemovedRelease: "1.25"}}
	return inv
}

// TestAPIUsageOutageKeepsFoldedCaller: a controller that both stores and
// calls a removed API is one blocker, the usage finding with the caller
// folded in. With api-usage unavailable the caller is a blocker of its
// own; it is the carried usage finding seen through the other
// capability, not news, and nor is the fold again when api-usage returns.
func TestAPIUsageOutageKeepsFoldedCaller(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("prod", withPSPCaller(testInventoryWithPSP()))
	expectNoEvents(t, h, "first evaluation")

	broken := withPSPCaller(testInventory())
	broken.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: false, Reason: "forbidden"}
	h.push("prod", broken)
	if c := h.fleetCell("prod", "1.35"); c == nil || c.Verdict != "blocked" {
		t.Fatalf("cell with api-usage unavailable = %+v, want blocked by the caller", c)
	}
	expectNoEvents(t, h, "api-usage failure")
	h.push("prod", broken)
	expectNoEvents(t, h, "second pass of the outage")

	h.push("prod", withPSPCaller(testInventoryWithPSP()))
	expectNoEvents(t, h, "api-usage back, PSP and its caller unchanged")

	h.push("prod", callsScraped(testInventory()))
	expectBecameReady(t, h, "PSP and its caller resolved")
}

// TestHelmOutageKeepsChartFoundAddOn: an add-on found through its Helm
// chart alone is gone from the inventory while helm is not assessed. That
// is not its resolution, and its return is not news (#189).
func TestHelmOutageKeepsChartFoundAddOn(t *testing.T) {
	h := newHarness(t, Config{KB: legacyKB()}, aug1)
	withHelm := func(inv inventory.Inventory) inventory.Inventory {
		inv.Capabilities[inventory.CapHelm] = inventory.CapabilityStatus{Available: true}
		return inv
	}
	h.push("prod", withHelm(argoChartInventory("2.10.0"))) // past its end of life
	if c := h.fleetCell("prod", "1.35"); c == nil || c.Verdict != "blocked" {
		t.Fatalf("cell with Argo CD 2.10 = %+v, want blocked", c)
	}
	expectNoEvents(t, h, "first evaluation")

	broken := argoChartInventory("2.10.0")
	broken.AddOns = nil
	broken.Capabilities[inventory.CapHelm] = inventory.CapabilityStatus{Available: false, Reason: "list secrets: forbidden"}
	h.push("prod", broken)
	if c := h.fleetCell("prod", "1.35"); c == nil || c.Verdict != "ready" {
		t.Fatalf("cell with helm unavailable = %+v, want the engine's verdict, ready", c)
	}
	expectNoEvents(t, h, "helm failure")

	h.push("prod", withHelm(argoChartInventory("2.10.0")))
	expectNoEvents(t, h, "helm back, Argo CD unchanged")

	h.push("prod", withHelm(argoChartInventory("3.1.0")))
	expectBecameReady(t, h, "Argo CD upgraded")
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

// TestKeepCarriedOverLimitKeepsTheEvaluation: carried heads that would
// take a report over maxReportBytes are not stored, rather than failing
// a push whose report fits: the next pass's baseline is then the
// report's own findings, as before carry-forward existed.
func TestKeepCarriedOverLimitKeepsTheEvaluation(t *testing.T) {
	const limit = 1 << 10
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.MaxSnapshotBytes = limit })
	carried := []findingHead{{Category: "deprecated-api-in-use", Severity: "blocker", Key: "deprecated-api-in-use/networking.k8s.io/v1beta1/servicecidrs"}}

	small := store.Evaluation{Report: []byte(`{"findings":[]}`)}
	if got := s.keepCarried(&small, carried); len(got) != 1 {
		t.Fatalf("kept %v, want the carried head", got)
	}
	if heads, err := storedFindingHeads(small.Report); err != nil || len(heads.CarriedForward) != 1 {
		t.Fatalf("stored heads = %+v, %v; want the carried head", heads, err)
	}

	report := []byte(`{"findings":[],"pad":"` + strings.Repeat("x", limit-40) + `"}`)
	near := store.Evaluation{Report: report}
	if got := s.keepCarried(&near, carried); got != nil {
		t.Fatalf("kept %v over the limit, want none", got)
	}
	if !bytes.Equal(near.Report, report) {
		t.Fatalf("report changed over the limit: %d bytes", len(near.Report))
	}
}

// scrapedAt is inv as scraped at collected from an apiserver started at
// started.
func scrapedAt(inv inventory.Inventory, started, collected time.Time) inventory.Inventory {
	inv.APIServerStartTime, inv.CollectedAt = started, collected
	return inv
}

// TestAPIServerRestartHoldsDeprecatedCallers: apiserver_requested_deprecated_apis
// starts empty when the apiserver restarts, so a caller missing from a
// scrape of an apiserver up for less than deprecatedCallsWarmup is not
// resolved, and its return is not news (#189, the red-team's case a).
func TestAPIServerRestartHoldsDeprecatedCallers(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	booted := aug1.Add(-72 * time.Hour)
	h.push("prod", scrapedAt(withServiceCIDRCaller(testInventoryWithPSP()), booted, aug1))
	expectNoEvents(t, h, "first evaluation")

	restarted := aug1.Add(30 * time.Minute)
	h.clock.set(restarted.Add(10 * time.Minute))
	h.push("prod", scrapedAt(callsScraped(testInventoryWithPSP()), restarted, h.clock.now()))
	expectNoEvents(t, h, "scrape after the restart")

	h.clock.set(restarted.Add(20 * time.Minute))
	h.push("prod", scrapedAt(withServiceCIDRCaller(testInventoryWithPSP()), restarted, h.clock.now()))
	expectNoEvents(t, h, "caller back after the restart")
}

// TestAPIServerRestartIsNotBecameReady: with a deprecated caller as the
// only blocker, an apiserver restart is not readiness (case c). The
// caller is resolved once a scrape of the restarted apiserver, up for
// deprecatedCallsWarmup, still does not show it: here the agent's hourly
// force-sync, a duplicate of the stored snapshot, after a midnight
// re-evaluation that still held it. Once, whatever is pushed after.
func TestAPIServerRestartIsNotBecameReady(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	booted := aug1.Add(-72 * time.Hour)
	h.push("prod", scrapedAt(withServiceCIDRCaller(testInventory()), booted, aug1))
	expectNoEvents(t, h, "first evaluation")

	restarted := aug1.Add(time.Hour)
	h.clock.set(restarted.Add(10 * time.Minute))
	quiet := callsScraped(testInventory())
	h.push("prod", scrapedAt(quiet, restarted, h.clock.now()))
	expectNoEvents(t, h, "scrape after the restart")

	h.clock.set(time.Date(2026, 8, 2, 0, 30, 0, 0, time.UTC))
	h.tick()
	expectNoEvents(t, h, "midnight re-evaluation within the window")

	h.clock.set(restarted.Add(deprecatedCallsWarmup + time.Hour))
	h.push("prod", scrapedAt(quiet, restarted, h.clock.now()))
	expectBecameReady(t, h, "force-sync past the window")

	h.clock.set(h.clock.now().Add(time.Hour))
	h.push("prod", scrapedAt(quiet, restarted, h.clock.now()))
	h.tick()
	expectNoEvents(t, h, "after became-ready")
}

// TestAPIServerUpLongEnoughResolvesCallers: a scrape of an apiserver up
// for deprecatedCallsWarmup is evidence, restart or not.
func TestAPIServerUpLongEnoughResolvesCallers(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("prod", scrapedAt(withServiceCIDRCaller(testInventory()), aug1.Add(-72*time.Hour), aug1))
	h.drain()

	h.clock.set(aug1.Add(time.Hour))
	h.push("prod", scrapedAt(callsScraped(testInventory()), h.clock.now().Add(-deprecatedCallsWarmup), h.clock.now()))
	expectBecameReady(t, h, "scrape of an apiserver up for the window")
}
