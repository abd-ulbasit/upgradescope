package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

func seedSnapshot(t *testing.T, st *fakeStore, inv inventory.Inventory) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	id, err := st.UpsertCluster(ctx, store.Cluster{Name: "c1", ClusterUID: inv.ClusterID, LastSeen: now})
	if err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	invJSON, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	if _, _, err := st.InsertSnapshot(ctx, store.Snapshot{
		ClusterID: id, Hash: "h1", ReceivedAt: now, Inventory: invJSON,
	}); err != nil {
		t.Fatalf("InsertSnapshot: %v", err)
	}
	return id
}

func TestWhatIfEvaluatesLatestSnapshot(t *testing.T) {
	st := newFakeStore()
	id := seedSnapshot(t, st, testInventoryWithPSP())
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	// PSP is removed in 1.35: target 1.35 → blocker; target 1.34 → warning
	// (removed exactly at target+1). The target visibly changes the verdict.
	rep, err := WhatIf(context.Background(), st, testKB(), nil, id, inventory.Version{Major: 1, Minor: 35}, now)
	if err != nil {
		t.Fatalf("WhatIf 1.35: %v", err)
	}
	if rep.ClusterID != "uid-123" || rep.KBVersion != "test-kb" {
		t.Fatalf("report identity = %q / %q, want uid-123 / test-kb", rep.ClusterID, rep.KBVersion)
	}
	if rep.Score != 75 || rep.Ready || len(rep.Findings) != 1 || rep.Findings[0].Severity != engine.SevBlocker {
		t.Fatalf("1.35 report = score %d ready %v findings %+v, want 75 false [1 blocker]", rep.Score, rep.Ready, rep.Findings)
	}

	rep34, err := WhatIf(context.Background(), st, testKB(), nil, id, inventory.Version{Major: 1, Minor: 34}, now)
	if err != nil {
		t.Fatalf("WhatIf 1.34: %v", err)
	}
	// 1.34 is the cluster's own minor, not an upgrade: the verdict is unknown.
	if rep34.Score != 95 || rep34.Verdict != engine.VerdictUnknown || len(rep34.Findings) != 1 || rep34.Findings[0].Severity != engine.SevWarning {
		t.Fatalf("1.34 report = score %d verdict %s findings %+v, want 95 unknown [1 warning]", rep34.Score, rep34.Verdict, rep34.Findings)
	}
}

// TestWhatIfEvaluatedAtIsUTC (#196 NEW-api-1): stored evaluations read
// back in UTC, but a what-if stamped the server clock's own zone, so one
// /fleet/teams answer mixed "...Z" with "...+05:00" on a non-UTC host.
// Every what-if read stamps UTC, whatever zone the clock is in.
func TestWhatIfEvaluatedAtIsUTC(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	pkt := time.FixedZone("PKT", 5*3600)
	s.now = func() time.Time { return time.Date(2026, 6, 10, 17, 0, 0, 0, pkt) }
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	pushCluster(t, ts, "alpha", testInventory()) // stored for 1.35 only

	var fleetTeams struct {
		Evaluated []struct {
			Source      string `json:"source"`
			EvaluatedAt string `json:"evaluatedAt"`
		} `json:"evaluated"`
	}
	getJSON(t, ts, "/api/v1/fleet/teams?target=1.37", "", &fleetTeams)
	if len(fleetTeams.Evaluated) != 1 || fleetTeams.Evaluated[0].Source != sourceWhatIf {
		t.Fatalf("/fleet/teams evaluated = %+v, want one what-if", fleetTeams.Evaluated)
	}
	stamps := map[string]string{"/fleet/teams": fleetTeams.Evaluated[0].EvaluatedAt}
	for _, path := range []string{"report", "teams", "findings"} {
		var meta struct {
			Source      string `json:"source"`
			EvaluatedAt string `json:"evaluatedAt"`
		}
		getJSON(t, ts, "/api/v1/clusters/1/"+path+"?target=1.37", "", &meta)
		if meta.Source != sourceWhatIf {
			t.Fatalf("/clusters/1/%s source = %q, want what-if", path, meta.Source)
		}
		stamps["/clusters/1/"+path] = meta.EvaluatedAt
	}
	for path, at := range stamps {
		if at != "2026-06-10T12:00:00Z" {
			t.Errorf("%s evaluatedAt = %q, want 2026-06-10T12:00:00Z (UTC)", path, at)
		}
	}
}

func TestWhatIfNoSnapshot(t *testing.T) {
	st := newFakeStore()
	_, err := WhatIf(context.Background(), st, testKB(), nil, 42, inventory.Version{Major: 1, Minor: 35}, time.Now())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound (wrapped)", err)
	}
}

func TestWhatIfCorruptStoredInventory(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	id, err := st.UpsertCluster(ctx, store.Cluster{Name: "c1", LastSeen: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertSnapshot(ctx, store.Snapshot{ClusterID: id, Hash: "h", Inventory: []byte("{not json")}); err != nil {
		t.Fatal(err)
	}
	_, err = WhatIf(ctx, st, testKB(), nil, id, inventory.Version{Major: 1, Minor: 35}, time.Now())
	if err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want non-nil non-NotFound corrupt-inventory error", err)
	}
}
