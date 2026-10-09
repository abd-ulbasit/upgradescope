package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// countingStore counts the calls that load a whole inventory
// (LatestSnapshot) and the passes (ListClusters, once per pass).
type countingStore struct {
	*fakeStore
	inventories, lists atomic.Int64
}

func (c *countingStore) LatestSnapshot(ctx context.Context, id int64) (store.Snapshot, error) {
	c.inventories.Add(1)
	return c.fakeStore.LatestSnapshot(ctx, id)
}

func (c *countingStore) ListClusters(ctx context.Context) ([]store.Cluster, error) {
	c.lists.Add(1)
	return c.fakeStore.ListClusters(ctx)
}

// pushNamedBody is the push of inv for the cluster called name.
func pushNamedBody(t *testing.T, name string, inv any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"schemaVersion": 1, "clusterName": name, "agentVersion": "test", "kbVersion": "test-kb", "inventory": inv})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func countedServer(t *testing.T) (*Server, *countingStore, *httptest.Server) {
	t.Helper()
	cs := &countingStore{fakeStore: newFakeStore()}
	s, err := New(Config{Store: cs, KB: testKB(), IngestToken: "ingest-tok"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, cs, ts
}

// TestReevaluationPassLoadsNoInventoryWhenNothingIsStale (#241): every
// pass decoded every cluster's whole inventory before it looked at
// whether anything was stale (23 of 24 hourly passes find nothing). It
// now reads the snapshot heads and the evaluation summaries first.
func TestReevaluationPassLoadsNoInventoryWhenNothingIsStale(t *testing.T) {
	s, cs, ts := countedServer(t)
	for i := range 3 {
		pushCluster(t, ts, fmt.Sprintf("c%d", i), testInventoryWithPSP())
	}
	cs.inventories.Store(0)
	s.reevaluateAll(context.Background())
	if n := cs.inventories.Load(); n != 0 {
		t.Errorf("a pass with nothing stale loaded %d inventories, want 0", n)
	}
	// A stale one is still re-evaluated, loading its inventory alone.
	s.cfg.KB.Version += "-new"
	s.reevaluateAll(context.Background())
	if n := cs.inventories.Load(); n != 3 {
		t.Errorf("a pass after a KB change loaded %d inventories, want 3 (each stale cluster once)", n)
	}
	for i := range 3 {
		c, err := cs.ClusterByName(context.Background(), fmt.Sprintf("c%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if e, err := cs.CurrentEvaluationSummary(context.Background(), c.ID, "1.35"); err != nil || e.KBVersion != s.cfg.KB.Version {
			t.Errorf("c%d after the pass: KB %q (%v), want %q", i, e.KBVersion, err, s.cfg.KB.Version)
		}
	}
}

// TestUnrefreshableRowDoesNotDrivePasses (#241): a row no pass can bring
// up to date (here its report would now be over --max-snapshot-bytes)
// stays outdated, and every read that served it started another pass,
// which loaded and evaluated the inventory again. The pass that fails
// marks it for its snapshot and the UTC day: reads still say outdated but
// start nothing, and later passes skip it until the snapshot or the day
// changes.
func TestUnrefreshableRowDoesNotDrivePasses(t *testing.T) {
	s, cs, ts := countedServer(t)
	pushCluster(t, ts, "prod", testInventoryWithPSP())
	s.cfg.MaxSnapshotBytes = 200
	s.cfg.KB.Version += "-new"
	cs.inventories.Store(0)
	s.reevaluateAll(context.Background())
	if n := cs.inventories.Load(); n != 1 {
		t.Fatalf("the first pass loaded %d inventories, want 1", n)
	}
	for range 100 {
		var f fleetView
		if resp := getJSON(t, ts, "/api/v1/fleet", "", &f); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /fleet = %d", resp.StatusCode)
		}
		if len(s.reevaluateKick) != 0 {
			t.Fatal("a read of the unrefreshable row started a pass")
		}
	}
	s.reevaluateAll(context.Background())
	if n := cs.inventories.Load(); n != 1 {
		t.Errorf("after the next pass: %d inventories loaded, want still 1", n)
	}
	// The next UTC day tries again, once.
	s.now = func() time.Time { return time.Date(2026, 6, 11, 0, 1, 0, 0, time.UTC) }
	s.reevaluateAll(context.Background())
	s.reevaluateAll(context.Background())
	if n := cs.inventories.Load(); n != 2 {
		t.Errorf("after two passes the next day: %d inventories loaded, want 2", n)
	}
}

// TestKickedPassesAreRateLimited: reads that serve an outdated row start
// the next pass early, but at most one per reevaluateCooldown after the
// last pass started, however many reads come in between.
func TestKickedPassesAreRateLimited(t *testing.T) {
	s, cs, ts := countedServer(t)
	pushCluster(t, ts, "prod", testInventoryWithPSP())
	s.reevaluateCooldown = 600 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.runReevaluation(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for cs.lists.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	for time.Since(start) < 300*time.Millisecond {
		s.kickReevaluation()
		time.Sleep(3 * time.Millisecond)
	}
	if n := cs.lists.Load(); n != 1 {
		t.Errorf("passes within the cooldown = %d, want 1 (the startup pass)", n)
	}
	time.Sleep(800 * time.Millisecond)
	if n := cs.lists.Load(); n != 2 {
		t.Errorf("passes after the cooldown = %d, want 2: one kicked pass for all the kicks in the window", n)
	}
}

// TestReadsLoadNoInventoryForTheServerVersion (#241): /history, a stored
// /report and the CSV export learned the default target by loading and
// scanning the whole inventory; the head's server version is enough.
func TestReadsLoadNoInventoryForTheServerVersion(t *testing.T) {
	_, cs, ts := countedServer(t)
	pushCluster(t, ts, "prod", testInventoryWithPSP())
	cs.inventories.Store(0)
	for _, path := range []string{
		"/api/v1/clusters/1/history",
		"/api/v1/clusters/1/report",
		"/api/v1/clusters/1/findings",
		"/api/v1/clusters/1/teams",
		"/api/v1/clusters/1/export?format=csv",
		"/api/v1/fleet",
		"/api/v1/clusters",
		"/metrics",
	} {
		if resp := getJSON(t, ts, path, "", nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		if n := cs.inventories.Swap(0); n != 0 {
			t.Errorf("GET %s loaded %d inventories, want 0", path, n)
		}
	}
}

// TestLegacyRowWithoutVersionIsReadOnce: a latest snapshot stored without
// a server version (before migration 0006, or never reported) is read
// whole once for it, not by every fleet read, and a duplicate push records
// the version on it.
func TestLegacyRowWithoutVersionIsReadOnce(t *testing.T) {
	_, cs, ts := countedServer(t)
	pushCluster(t, ts, "prod", testInventoryWithPSP())
	cs.mu.Lock()
	cs.snapshots[0].ServerVersion = "" // as stored before 0006
	cs.mu.Unlock()
	cs.inventories.Store(0)
	for range 5 {
		var f fleetView
		if resp := getJSON(t, ts, "/api/v1/fleet", "", &f); resp.StatusCode != http.StatusOK || len(f.Clusters) != 1 || f.Clusters[0].ServerVersion != "v1.34.2" {
			t.Fatalf("GET /fleet = %d %+v, want the inventory's version", resp.StatusCode, f)
		}
	}
	if n := cs.inventories.Load(); n != 1 {
		t.Errorf("5 fleet reads of a legacy row loaded %d inventories, want 1", n)
	}
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushNamedBody(t, "prod", testInventoryWithPSP()), false); resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate push = %d %v, want 200", resp.StatusCode, out)
	}
	if head, err := cs.LatestSnapshotHead(context.Background(), 1); err != nil || head.ServerVersion != "v1.34.2" {
		t.Errorf("after a duplicate push: ServerVersion = (%q, %v), want v1.34.2 recorded", head.ServerVersion, err)
	}
}

// TestPassLoadsTheInventoryOfAHeldTarget (SV-17): a target whose current
// evaluation carries a held deprecated caller (after an apiserver restart,
// #204) is not skipped by the pass although nothing is stale: only the
// inventory says whether the scrape it judges ends or moves the hold
// (holdChanged), and the summary alone cannot. A target without a held
// caller, in the same state, loads nothing.
func TestPassLoadsTheInventoryOfAHeldTarget(t *testing.T) {
	cs := &countingStore{fakeStore: newFakeStore()}
	s, err := New(Config{Store: cs, KB: testKB(), IngestToken: "ingest-tok", Notifier: &recordingNotifier{}})
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: aug1}
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	push := func(name string, at time.Time, inv inventory.Inventory) {
		t.Helper()
		clock.set(at)
		if resp, out := postSnapshot(t, ts, "ingest-tok", pushNamedBody(t, name, inv), false); resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
			t.Fatalf("push %s at %v: %d %v", name, at, resp.StatusCode, out)
		}
	}
	// "held": its caller was seen, then the apiserver restarted and the
	// next scrape has none, so the caller is held for the window.
	push("held", aug1, scrapedAt(withServiceCIDRCaller(testInventory()), aug1, oldStart))
	restart := aug1.Add(time.Hour)
	at := restart.Add(5 * time.Minute)
	push("held", at, scrapedAt(callsScraped(testInventory()), at, restart))
	// "plain": the same state without any caller.
	push("plain", at, scrapedAt(callsScraped(testInventory()), at, oldStart))

	ctx := context.Background()
	for name, want := range map[string]bool{"held": true, "plain": false} {
		c, err := cs.ClusterByName(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		e, err := cs.CurrentEvaluationSummary(ctx, c.ID, "1.35")
		if err != nil || e.CarriesHold != want || s.stale(e, clock.now()) {
			t.Fatalf("%s: summary CarriesHold=%v stale=%v (%v), want CarriesHold=%v on a fresh row", name, e.CarriesHold, s.stale(e, clock.now()), err, want)
		}
	}

	cs.inventories.Store(0)
	s.reevaluateAll(ctx)
	if n := cs.inventories.Load(); n != 1 {
		t.Errorf("a pass over one held and one plain cluster loaded %d inventories, want 1 (the held one)", n)
	}
}

// busyCommitStore fails CommitEvaluations with a transient error while
// busy is set (SQLITE_BUSY, a dropped Postgres connection).
type busyCommitStore struct {
	*countingStore
	busy atomic.Bool
}

func (b *busyCommitStore) CommitEvaluations(ctx context.Context, batch store.EvaluationBatch) (int64, bool, error) {
	if b.busy.Load() {
		return 0, false, errors.New("database is locked")
	}
	return b.countingStore.CommitEvaluations(ctx, batch)
}

// TestTransientStoreErrorDoesNotMarkTheClusterUnrefreshable: only what
// repeats for the same snapshot (a report over the limit, a corrupt
// inventory, a failed evaluation) marks a cluster; one store error in a
// pass must not leave the cluster skipped by passes, and its reads
// without a pass of their own, until the next UTC day.
func TestTransientStoreErrorDoesNotMarkTheClusterUnrefreshable(t *testing.T) {
	cs := &busyCommitStore{countingStore: &countingStore{fakeStore: newFakeStore()}}
	s, err := New(Config{Store: cs, KB: testKB(), IngestToken: "ingest-tok"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	pushCluster(t, ts, "prod", testInventoryWithPSP())
	head, err := cs.LatestSnapshotHead(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	s.cfg.KB.Version += "-new"
	cs.busy.Store(true)
	s.reevaluateAll(context.Background())
	if s.unrefreshable.has(1, head.ID, s.now()) {
		t.Fatal("a store error in the pass marked the cluster unrefreshable")
	}
	e, err := cs.CurrentEvaluationSummary(context.Background(), 1, "1.35")
	if err != nil || !s.outdated(e, s.now()) || len(s.reevaluateKick) != 1 {
		t.Fatalf("read of the outdated row after the blip: outdated and kicking a pass expected, kicks = %d (%v)", len(s.reevaluateKick), err)
	}

	// The next pass, with the store back, brings it up to date.
	cs.busy.Store(false)
	s.reevaluateAll(context.Background())
	if e, err := cs.CurrentEvaluationSummary(context.Background(), 1, "1.35"); err != nil || e.KBVersion != s.cfg.KB.Version {
		t.Errorf("after the store came back: KB %q (%v), want %q", e.KBVersion, err, s.cfg.KB.Version)
	}
}

// TestHoldMarkerIsTheCarriedFieldName: the carries_hold column and
// holdChanged both ask store.CarriesHold of a stored report, so its marker
// must be what a carried head with a hold encodes as.
func TestHoldMarkerIsTheCarriedFieldName(t *testing.T) {
	b, err := json.Marshal(findingHead{Key: "k", HoldUntil: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if !store.CarriesHold(b) {
		t.Errorf("a head with a hold encodes as %s, without the hold marker", b)
	}
	if b, _ := json.Marshal(findingHead{Key: "k"}); store.CarriesHold(b) {
		t.Errorf("a head without a hold encodes as %s, with the marker", b)
	}
}

// corruptingStore serves a stored inventory that does not decode while
// corrupt is set.
type corruptingStore struct {
	*countingStore
	corrupt atomic.Bool
}

func (c *corruptingStore) LatestSnapshot(ctx context.Context, id int64) (store.Snapshot, error) {
	snap, err := c.countingStore.LatestSnapshot(ctx, id)
	if err == nil && c.corrupt.Load() {
		snap.Inventory = []byte(`{"serverVersion": "v1.34.0", "namespaces": [`)
	}
	return snap, err
}

// outdatedClusterIsMarked asserts what a failed pass leaves for the
// cluster's "1.35" row: still outdated, marked, not loaded again by later
// passes of the day, and without a pass started by its reads. Passes try
// again the next UTC day.
func outdatedClusterIsMarked(t *testing.T, s *Server, store store.Store, loads *atomic.Int64) {
	t.Helper()
	head, err := store.LatestSnapshotHead(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !s.unrefreshable.has(1, head.ID, s.now()) {
		t.Fatal("the failed pass did not mark the cluster unrefreshable")
	}
	e, err := store.CurrentEvaluationSummary(context.Background(), 1, "1.35")
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if !s.outdated(e, s.now()) {
			t.Fatal("the row the pass could not bring up to date is not served as outdated")
		}
	}
	if n := len(s.reevaluateKick); n != 0 {
		t.Errorf("reads of the marked row started %d passes, want none", n)
	}
	before := loads.Load()
	s.reevaluateAll(context.Background())
	if n := loads.Load(); n != before {
		t.Errorf("a later pass of the same day loaded %d more inventories, want 0 (the cluster is skipped)", n-before)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 11, 0, 1, 0, 0, time.UTC) }
	s.reevaluateAll(context.Background())
	if n := loads.Load(); n != before+1 {
		t.Errorf("the next UTC day's pass loaded %d inventories, want 1 (it tries again)", n-before)
	}
}

// TestCorruptInventoryMarksTheClusterUnrefreshable (#241, SV-03b): a
// stored inventory that does not decode repeats for the same snapshot, so
// the pass marks the cluster: later passes skip it, and its reads kick
// nothing, until the snapshot or the UTC day changes.
func TestCorruptInventoryMarksTheClusterUnrefreshable(t *testing.T) {
	cs := &corruptingStore{countingStore: &countingStore{fakeStore: newFakeStore()}}
	s, err := New(Config{Store: cs, KB: testKB(), IngestToken: "ingest-tok"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	pushCluster(t, ts, "prod", testInventoryWithPSP())

	s.cfg.KB.Version += "-new"
	cs.corrupt.Store(true)
	cs.inventories.Store(0)
	s.reevaluateAll(context.Background())
	if n := cs.inventories.Load(); n != 1 {
		t.Fatalf("the first pass loaded %d inventories, want 1", n)
	}
	outdatedClusterIsMarked(t, s, cs, &cs.inventories)
}

// TestFailedEvaluationMarksTheClusterUnrefreshable (#241, SV-03b): an
// evaluation that fails for a reason other than the store's (nothing but
// a too-large report fails the real engine, which is handled apart, so
// the engine is replaced) repeats for the same snapshot: the pass marks
// the cluster, as it does a corrupt inventory.
func TestFailedEvaluationMarksTheClusterUnrefreshable(t *testing.T) {
	s, cs, ts := countedServer(t)
	pushCluster(t, ts, "prod", testInventoryWithPSP())

	s.cfg.KB.Version += "-new"
	s.runEngine = func(inventory.Inventory, kb.KB, inventory.Version, time.Time, int) (engine.Report, error) {
		return engine.Report{}, errors.New("engine failed")
	}
	cs.inventories.Store(0)
	s.reevaluateAll(context.Background())
	if n := cs.inventories.Load(); n != 1 {
		t.Fatalf("the first pass loaded %d inventories, want 1", n)
	}
	outdatedClusterIsMarked(t, s, cs, &cs.inventories)
}
