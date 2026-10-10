package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// An apiserver restart empties apiserver_requested_deprecated_apis, so a
// deprecated caller missing from the next scrapes is not yet evidence of
// a fix (#204). These tests push through the real ingest on SQLite, as
// the #189 tests do (testKB: the servicecidrs caller blocks target 1.35),
// with the agent's clock (CollectedAt), the apiserver's start time and
// the server's clock set apart.

// oldStart is when the apiserver that counted the caller started: long
// before any hold.
var oldStart = aug1.Add(-30 * 24 * time.Hour)

// scrapedAt is inv collected at collected by the agent's clock, its
// /metrics read from an apiserver started at start (zero: a scrape that
// did not report it, or an agent that predates the field).
func scrapedAt(inv inventory.Inventory, collected, start time.Time) inventory.Inventory {
	inv.CollectedAt, inv.APIServerStartTime = collected, start
	return inv
}

// pushAt pushes inv at server time now and returns the status.
func (h *harness) pushAt(now time.Time, inv inventory.Inventory) int {
	h.t.Helper()
	h.clock.set(now)
	code, out := h.push("prod", inv)
	if code != http.StatusAccepted && code != http.StatusOK {
		h.t.Fatalf("push at %v: status %d %v", now, code, out)
	}
	return code
}

func (h *harness) historyPoints(target string) int {
	h.t.Helper()
	points, err := h.st.ScoreHistory(context.Background(), h.clusterID("prod"), target, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	return len(points)
}

// TestRestartHoldEndsOnDuplicatePushes: the apiserver restarts late in a
// UTC day, the caller (the only blocker) is missing from every scrape
// after it, and the agent sends only its hourly force-sync duplicates.
// Nothing is sent inside the window, the midnight passes (one inside it,
// one after its end) re-evaluate the stored snapshot, scraped inside it,
// without ending it or stopping a later push from ending it, and the
// first duplicate scraped after the window end sends exactly one
// became-ready: within deprecatedCallsHold plus one force-sync interval
// of the restart.
func TestRestartHoldEndsOnDuplicatePushes(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")

	restart := time.Date(2026, 8, 1, 23, 50, 0, 0, time.UTC)
	end := restart.Add(deprecatedCallsHold)
	gone := callsScraped(testInventory())
	first := restart.Add(5 * time.Minute)
	if code := h.pushAt(first, scrapedAt(gone, first, restart)); code != http.StatusAccepted {
		t.Fatalf("first scrape after the restart: status %d, want a new snapshot", code)
	}
	expectNoEvents(t, h, "first scrape after the restart")

	// The UTC midnight pass inside the window, then hourly duplicates,
	// each scraped by the agent at the push.
	h.clock.set(time.Date(2026, 8, 2, 0, 0, 1, 0, time.UTC))
	h.tick()
	expectNoEvents(t, h, "midnight pass inside the window")
	at := time.Date(2026, 8, 2, 0, 10, 0, 0, time.UTC)
	for ; at.Before(end); at = at.Add(time.Hour) {
		if code := h.pushAt(at, scrapedAt(gone, at, restart)); code != http.StatusOK {
			t.Fatalf("force-sync at %v: status %d, want a duplicate", at, code)
		}
		expectNoEvents(t, h, "force-sync at "+at.String())
	}

	// The midnight pass after the window end (23:50) and before the next
	// force-sync (00:10) judges the stored snapshot, scraped inside the
	// window: still held, and it stamps the row evaluated after the end.
	h.clock.set(time.Date(2026, 8, 3, 0, 0, 1, 0, time.UTC))
	h.tick()
	expectNoEvents(t, h, "midnight pass after the window end")

	if bound := restart.Add(deprecatedCallsHold + time.Hour); at.After(bound) {
		t.Fatalf("test timeline: the push at %v is past the bound %v", at, bound)
	}
	h.pushAt(at, scrapedAt(gone, at, restart))
	expectBecameReady(t, h, "first force-sync after the window end")

	for i := 1; i <= 3; i++ {
		at := at.Add(time.Duration(i) * time.Hour)
		h.pushAt(at, scrapedAt(gone, at, restart))
		expectNoEvents(t, h, "force-sync after became-ready")
	}
	h.clock.set(time.Date(2026, 8, 4, 0, 0, 1, 0, time.UTC))
	h.tick()
	expectNoEvents(t, h, "midnight pass after became-ready")
}

// TestRestartHoldKeepsCallerWithOtherBlocker: with PSP still blocking,
// the caller missing after a restart is not resolved, so its next call
// inside the window is not a new blocker (#189's metric-reset flap).
func TestRestartHoldKeepsCallerWithOtherBlocker(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventoryWithPSP()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")

	restart := aug1.Add(time.Hour)
	at := restart.Add(5 * time.Minute)
	h.pushAt(at, scrapedAt(callsScraped(testInventoryWithPSP()), at, restart))
	expectNoEvents(t, h, "scrape after the restart")

	at = restart.Add(6 * time.Hour)
	h.pushAt(at, scrapedAt(withServiceCIDRCaller(testInventoryWithPSP()), at, restart))
	expectNoEvents(t, h, "the caller calls again inside the window")
}

// TestRestartHoldSurvivesKBRefresh: a knowledge-base update inside the
// window re-evaluates the stored snapshot on the server's startup pass:
// still held, and the first scrape after the window end resolves it once.
// The refresh and every push after it are on one UTC day, so the row is
// never stale by date after the refresh: only the hold's end
// (holdChanged) re-evaluates it.
func TestRestartHoldSurvivesKBRefresh(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")

	restart := aug1.Add(time.Hour)
	end := restart.Add(deprecatedCallsHold)
	gone := callsScraped(testInventory())
	at := restart.Add(5 * time.Minute)
	h.pushAt(at, scrapedAt(gone, at, restart))
	expectNoEvents(t, h, "scrape after the restart")

	refreshed := testKB()
	refreshed.Version = "test-kb-2"
	refreshAt, after := end.Add(-2*time.Hour), end.Add(30*time.Minute)
	if day := 24 * time.Hour; !refreshAt.Truncate(day).Equal(after.Truncate(day)) {
		t.Fatalf("test timeline: the refresh at %v and the push at %v are on different UTC days", refreshAt, after)
	}
	h.clock.set(refreshAt)
	h.restart(Config{KB: refreshed})
	h.tick() // the startup pass: every row is stale for the new KB
	expectNoEvents(t, h, "KB refresh inside the window")
	cur, err := h.st.CurrentEvaluation(context.Background(), h.clusterID("prod"), "1.35")
	if err != nil || cur.KBVersion != refreshed.Version {
		t.Fatalf("current evaluation after the refresh = %+v, %v; want one under %s", cur, err, refreshed.Version)
	}

	at = end.Add(-time.Hour)
	h.pushAt(at, scrapedAt(gone, at, restart))
	expectNoEvents(t, h, "force-sync inside the window, after the refresh")

	h.pushAt(after, scrapedAt(gone, after, restart))
	expectBecameReady(t, h, "first force-sync after the window end")
}

// TestRestartHoldUsesTheAgentsClock: the server's clock runs three days
// ahead of the agent's. The window is measured on the scrape (the agent's
// CollectedAt against the apiserver's start time), so the server's clock
// neither ends the hold early nor holds past the window.
func TestRestartHoldUsesTheAgentsClock(t *testing.T) {
	const ahead = 72 * time.Hour
	h := newHarness(t, Config{KB: testKB()}, aug1.Add(ahead))
	h.pushAt(aug1.Add(ahead), scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")

	restart := aug1.Add(time.Hour)
	end := restart.Add(deprecatedCallsHold)
	gone := callsScraped(testInventory())
	for at := restart.Add(5 * time.Minute); at.Before(end); at = at.Add(6 * time.Hour) {
		h.pushAt(at.Add(ahead), scrapedAt(gone, at, restart))
		expectNoEvents(t, h, "scrape inside the window by the agent's clock")
	}
	h.clock.set(end.Add(ahead + 24*time.Hour))
	h.tick()
	expectNoEvents(t, h, "midnight pass by the server's clock")

	at := end.Add(10 * time.Minute)
	h.pushAt(at.Add(ahead+24*time.Hour), scrapedAt(gone, at, restart))
	expectBecameReady(t, h, "first scrape after the window end by the agent's clock")
}

// TestAPIServerMoveIsNotANewSnapshot: an agent whose connection moves
// between HA apiservers, started at different times, sends the same
// inventory but for the start time: every push is a duplicate, with no
// new snapshot, history point or notification.
func TestAPIServerMoveIsNotANewSnapshot(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")
	points, first := h.historyPoints("1.35"), h.latestSnapshotID()

	starts := []time.Time{oldStart.Add(time.Hour), oldStart, aug1.Add(-time.Hour), oldStart.Add(time.Hour)}
	for i, start := range starts {
		at := aug1.Add(time.Duration(i+1) * 10 * time.Minute)
		if code := h.pushAt(at, scrapedAt(withServiceCIDRCaller(testInventory()), at, start)); code != http.StatusOK {
			t.Fatalf("push %d from the apiserver started at %v: status %d, want a duplicate", i, start, code)
		}
		expectNoEvents(t, h, "HA move")
	}
	if got := h.historyPoints("1.35"); got != points {
		t.Fatalf("history points after HA moves = %d, want %d", got, points)
	}
	if latest := h.latestSnapshotID(); latest != first {
		t.Fatalf("latest snapshot after HA moves = %d, want %d", latest, first)
	}
}

func (h *harness) latestSnapshotID() int64 {
	h.t.Helper()
	snap, err := h.st.LatestSnapshotHead(context.Background(), h.clusterID("prod"))
	if err != nil {
		h.t.Fatal(err)
	}
	return snap.ID
}

// TestImplausibleAPIServerStartIsIgnored: a start time later than the
// scrape (beyond startTimeTolerance) or before Kubernetes existed is
// treated as absent, so it holds nothing: the caller is resolved as for
// an agent that does not report it. One just after the scrape, within the
// tolerance, is an apiserver that started while the agent collected. The
// pre-2014 scrape is an hour after its start, so only that check keeps it
// from holding.
func TestImplausibleAPIServerStartIsIgnored(t *testing.T) {
	pre2014 := time.Date(2013, 12, 31, 0, 0, 0, 0, time.UTC)
	for name, scrape := range map[string]func(pushed time.Time) (collected, start time.Time){
		"a day after the scrape": func(p time.Time) (time.Time, time.Time) { return p, p.Add(24 * time.Hour) },
		"years after the scrape": func(p time.Time) (time.Time, time.Time) { return p, p.Add(10 * 365 * 24 * time.Hour) },
		"before 2014":            func(time.Time) (time.Time, time.Time) { return pre2014.Add(time.Hour), pre2014 },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{KB: testKB()}, aug1)
			h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
			expectNoEvents(t, h, "first evaluation")

			at := aug1.Add(time.Hour)
			collected, start := scrape(at)
			h.pushAt(at, scrapedAt(callsScraped(testInventory()), collected, start))
			expectBecameReady(t, h, "scrape with an implausible start time")
		})
	}

	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")
	at := aug1.Add(time.Hour)
	h.pushAt(at, scrapedAt(callsScraped(testInventory()), at, at.Add(startTimeTolerance/2)))
	expectNoEvents(t, h, "apiserver started during the collection")
}

// TestRestartHoldCarriesItsEnd: the hold's end is recorded on the carried
// caller, the apiserver's start plus deprecatedCallsHold, and a later
// restart inside the window moves it to that apiserver's.
func TestRestartHoldCarriesItsEnd(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventoryWithPSP()), aug1, oldStart))
	held := func() time.Time {
		t.Helper()
		cur, err := h.st.CurrentEvaluation(context.Background(), h.clusterID("prod"), "1.35")
		if err != nil {
			t.Fatal(err)
		}
		heads, err := storedFindingHeads(cur.Report)
		if err != nil || len(heads.CarriedForward) != 1 {
			t.Fatalf("stored heads = %+v, %v; want the caller carried", heads, err)
		}
		return heads.CarriedForward[0].HoldUntil
	}

	first := aug1.Add(time.Hour)
	h.pushAt(first.Add(time.Minute), scrapedAt(callsScraped(testInventoryWithPSP()), first.Add(time.Minute), first))
	if got, want := held(), first.Add(deprecatedCallsHold); !got.Equal(want) {
		t.Fatalf("hold end = %v, want %v", got, want)
	}

	second := first.Add(5 * time.Hour)
	h.pushAt(second.Add(time.Minute), scrapedAt(callsScraped(testInventoryWithPSP()), second.Add(time.Minute), second))
	if got, want := held(), second.Add(deprecatedCallsHold); !got.Equal(want) {
		t.Fatalf("hold end after a second restart = %v, want %v", got, want)
	}
	expectNoEvents(t, h, "inside the window")

	at := second.Add(deprecatedCallsHold)
	h.pushAt(at, scrapedAt(callsScraped(testInventoryWithPSP()), at, second))
	expectNoEvents(t, h, "the caller resolved while PSP remains")
	h.pushAt(at.Add(time.Hour), scrapedAt(withServiceCIDRCaller(testInventoryWithPSP()), at.Add(time.Hour), second))
	if evs := h.drain(); len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker {
		t.Fatalf("the caller calls after the window: events = %+v, want exactly one new-blocker", evs)
	}
}

// TestRestartHoldFromAFutureClockEnds: a single-node cluster boots with
// its clock years ahead, so one scrape's collectedAt and apiserver start
// agree, pass the start-time check and record a hold end years away. Once
// the clock is corrected, a later scrape cannot have recorded that end
// (none ends more than deprecatedCallsHold plus startTimeTolerance after
// its scrape), so it is dropped: the caller, PSP gone, is resolved with
// exactly one became-ready within deprecatedCallsHold plus one
// force-sync interval of the real restart.
func TestRestartHoldFromAFutureClockEnds(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, scrapedAt(withServiceCIDRCaller(testInventoryWithPSP()), aug1, oldStart))
	expectNoEvents(t, h, "first evaluation")

	future := aug1.Add(4 * 365 * 24 * time.Hour)
	h.pushAt(aug1.Add(time.Hour), scrapedAt(callsScraped(testInventoryWithPSP()), future.Add(5*time.Minute), future))
	expectNoEvents(t, h, "scrape by a clock years ahead")

	restart := aug1.Add(time.Hour)
	bound := restart.Add(deprecatedCallsHold + time.Hour)
	gone := callsScraped(testInventory())
	ready := 0
	for at := restart.Add(10 * time.Minute); at.Before(restart.Add(72 * time.Hour)); at = at.Add(time.Hour) {
		if at.Hour() == 0 { // the UTC midnight pass first
			h.clock.set(at.Truncate(24 * time.Hour).Add(time.Second))
			h.tick()
		}
		h.pushAt(at, scrapedAt(gone, at, restart))
		for _, ev := range h.drain() {
			if ev.Kind != notify.KindBecameReady {
				t.Fatalf("push at %v: event %+v, want only a became-ready", at, ev)
			}
			if at.Before(restart.Add(deprecatedCallsHold)) || at.After(bound) {
				t.Fatalf("became-ready at %v, want one between the window end %v and %v", at, restart.Add(deprecatedCallsHold), bound)
			}
			ready++
		}
	}
	if ready != 1 {
		t.Fatalf("became-ready events after the clock was corrected = %d, want exactly 1", ready)
	}
}

// The age of the pod evidence an agent reused (#228) grows with every tick
// that reuses a pass: a push that differs only in it is a duplicate, and
// the snapshot keeps the age it was pushed with.
func TestAddOnEvidenceAgeIsNotANewSnapshot(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.pushAt(aug1, testInventory())
	points, first := h.historyPoints("1.35"), h.latestSnapshotID()
	for i, age := range []int64{600, 1200, 1800} {
		at := aug1.Add(time.Duration(i+1) * 10 * time.Minute)
		inv := testInventory()
		inv.AddOnEvidenceAgeSeconds = age
		if code := h.pushAt(at, inv); code != http.StatusOK {
			t.Fatalf("push with age %d: status %d, want a duplicate", age, code)
		}
	}
	if got := h.historyPoints("1.35"); got != points {
		t.Fatalf("history points = %d, want %d", got, points)
	}
	if latest := h.latestSnapshotID(); latest != first {
		t.Fatalf("latest snapshot = %d, want %d", latest, first)
	}
}
