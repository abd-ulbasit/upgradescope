package server

import (
	"context"
	"encoding/json"
	"errors"
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
	rep, err := WhatIf(context.Background(), st, testKB(), id, inventory.Version{Major: 1, Minor: 35}, now)
	if err != nil {
		t.Fatalf("WhatIf 1.35: %v", err)
	}
	if rep.ClusterID != "uid-123" || rep.KBVersion != "test-kb" {
		t.Fatalf("report identity = %q / %q, want uid-123 / test-kb", rep.ClusterID, rep.KBVersion)
	}
	if rep.Score != 75 || rep.Ready || len(rep.Findings) != 1 || rep.Findings[0].Severity != engine.SevBlocker {
		t.Fatalf("1.35 report = score %d ready %v findings %+v, want 75 false [1 blocker]", rep.Score, rep.Ready, rep.Findings)
	}

	rep34, err := WhatIf(context.Background(), st, testKB(), id, inventory.Version{Major: 1, Minor: 34}, now)
	if err != nil {
		t.Fatalf("WhatIf 1.34: %v", err)
	}
	if rep34.Score != 95 || !rep34.Ready || len(rep34.Findings) != 1 || rep34.Findings[0].Severity != engine.SevWarning {
		t.Fatalf("1.34 report = score %d ready %v findings %+v, want 95 true [1 warning]", rep34.Score, rep34.Ready, rep34.Findings)
	}
}

func TestWhatIfNoSnapshot(t *testing.T) {
	st := newFakeStore()
	_, err := WhatIf(context.Background(), st, testKB(), 42, inventory.Version{Major: 1, Minor: 35}, time.Now())
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
	_, err = WhatIf(ctx, st, testKB(), id, inventory.Version{Major: 1, Minor: 35}, time.Now())
	if err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want non-nil non-NotFound corrupt-inventory error", err)
	}
}
