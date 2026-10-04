package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// The restart hold (notify_restart_test.go) meets a control-plane upgrade
// and a mix of agents (#204).

// serviceCIDRKB is testKB that also knows v1beta1 ServiceCIDR, removed in
// 1.35 (the metric row withServiceCIDRCaller adds says so too). That
// removal is the fixture's, to put the upgrade from the harness's 1.34
// cluster on a removal; the shipped knowledge base removes it in 1.37
// (TestRemovalOfCallShippedKB), so docs use a real removal for an example.
func serviceCIDRKB() kb.KB {
	k := testKB()
	deprecated, removed := inventory.Version{Major: 1, Minor: 33}, inventory.Version{Major: 1, Minor: 35}
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Group: "networking.k8s.io", Version: "v1beta1", Kind: "ServiceCIDR",
		Introduced: inventory.Version{Major: 1, Minor: 31}, Deprecated: &deprecated, Removed: &removed,
	})
	return k
}

// atServer is inv scraped from an apiserver running version.
func atServer(inv inventory.Inventory, version string) inventory.Inventory {
	inv.ServerVersion = version
	return inv
}

// carriedHold is the hold end recorded on the one carried caller of
// prod's evaluation for target.
func (h *harness) carriedHold(target string) time.Time {
	h.t.Helper()
	cur, err := h.st.CurrentEvaluation(context.Background(), h.clusterID("prod"), target)
	if err != nil {
		h.t.Fatal(err)
	}
	heads, err := storedFindingHeads(cur.Report)
	if err != nil || len(heads.CarriedForward) != 1 {
		h.t.Fatalf("stored heads = %+v, %v; want the caller carried", heads, err)
	}
	return heads.CarriedForward[0].HoldUntil
}

// TestRestartHoldSkipsAnAPIThatUpgradeRemoved: the control plane upgrades
// from v1.34.2 to v1.35.1, which removes the API the baseline's only
// blocker was a caller of. The upgrade is an apiserver restart, so the
// scrape is young and lacks the caller, but v1.35 no longer serves the
// API: nothing can call it, and the gauge can never count it again. It is
// resolved at once, with exactly one became-ready, not held for a day.
func TestRestartHoldSkipsAnAPIThatUpgradeRemoved(t *testing.T) {
	h := newHarness(t, Config{KB: serviceCIDRKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")

	upgraded := aug1.Add(2 * time.Hour)
	at := upgraded.Add(5 * time.Minute)
	h.pushAt(at, scrapedAt(atServer(callsScraped(testInventory()), "v1.35.1"), at, upgraded))
	expectBecameReady(t, h, "first scrape of the upgraded apiserver")
	h.pushAt(at.Add(time.Hour), scrapedAt(atServer(callsScraped(testInventory()), "v1.35.1"), at.Add(time.Hour), upgraded))
	expectNoEvents(t, h, "force-sync after became-ready")
}

// TestRestartHoldKeepsAnAPIThePatchStillServes: the same restart without
// the removal, a patch upgrade to v1.34.3, is held as any restart is, and
// resolved by the first scrape after the window end.
func TestRestartHoldKeepsAnAPIThePatchStillServes(t *testing.T) {
	h := newHarness(t, Config{KB: serviceCIDRKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")

	restart := aug1.Add(2 * time.Hour)
	end := restart.Add(deprecatedCallsHold)
	gone := atServer(callsScraped(testInventory()), "v1.34.3")
	at := restart.Add(5 * time.Minute)
	h.pushAt(at, scrapedAt(gone, at, restart))
	expectNoEvents(t, h, "first scrape after the patch upgrade")
	at = end.Add(-time.Hour)
	h.pushAt(at, scrapedAt(gone, at, restart))
	expectNoEvents(t, h, "force-sync inside the window")
	at = end.Add(10 * time.Minute)
	h.pushAt(at, scrapedAt(gone, at, restart))
	expectBecameReady(t, h, "first scrape after the window end")
}

// TestRestartHoldFromAnOldAgentKeepsTheCarriedEnd: a new agent's scrape
// after a restart records the hold's end; later pushes come from an agent
// that predates the start time (a rollback, or another replica), whose
// scrapes carry none. They are duplicates of the snapshot, judged with
// their own collectedAt: the carried end is honoured, so nothing is
// resolved inside the window, and the hold stays bounded: the first scrape
// after the end resolves it, once.
func TestRestartHoldFromAnOldAgentKeepsTheCarriedEnd(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")

	restart := aug1.Add(2 * time.Hour)
	end := restart.Add(deprecatedCallsHold)
	gone := callsScraped(testInventory())
	at := restart.Add(5 * time.Minute)
	h.pushAt(at, scrapedAt(gone, at, restart))
	expectNoEvents(t, h, "new agent's scrape after the restart")

	for at = at.Add(time.Hour); at.Before(end); at = at.Add(time.Hour) {
		if code := h.pushAt(at, scrapedAt(gone, at, time.Time{})); code != http.StatusOK {
			t.Fatalf("old agent's push at %v: status %d, want a duplicate", at, code)
		}
		expectNoEvents(t, h, "old agent's push at "+at.String())
		if got := h.carriedHold("1.35"); !got.Equal(end) {
			t.Fatalf("after the old agent's push at %v: hold end = %v, want %v", at, got, end)
		}
	}
	if bound := end.Add(time.Hour); at.After(bound) {
		t.Fatalf("test timeline: the push at %v is past the bound %v", at, bound)
	}
	h.pushAt(at, scrapedAt(gone, at, time.Time{}))
	expectBecameReady(t, h, "old agent's first push after the window end")
	h.pushAt(at.Add(time.Hour), scrapedAt(gone, at.Add(time.Hour), time.Time{}))
	expectNoEvents(t, h, "push after became-ready")
}

// TestBackgroundPassKeepsALaterRecordedEnd: a second restart inside the
// window produces only duplicate pushes, which move the carried end to the
// new apiserver's. The background pass judges the stored snapshot, scraped
// before both restarts; it neither drops that later end as unrecordable
// nor stamps the older one back, which would cost a write at every pass.
func TestBackgroundPassKeepsALaterRecordedEnd(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))

	first := aug1.Add(time.Hour)
	gone := callsScraped(testInventory())
	at := first.Add(5 * time.Minute)
	h.pushAt(at, scrapedAt(gone, at, first))
	second := first.Add(8 * time.Hour)
	at = second.Add(5 * time.Minute)
	if code := h.pushAt(at, scrapedAt(gone, at, second)); code != http.StatusOK {
		t.Fatalf("scrape after the second restart: status %d, want a duplicate", code)
	}
	expectNoEvents(t, h, "second restart inside the window")

	h.clock.set(at.Add(time.Hour))
	h.tick()
	expectNoEvents(t, h, "background pass")
	if got, want := h.carriedHold("1.35"), second.Add(deprecatedCallsHold); !got.Equal(want) {
		t.Fatalf("hold end after the background pass = %v, want %v", got, want)
	}
}
