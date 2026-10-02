package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// TestFleetDecodesNoInventories: /fleet reads each cluster's latest
// snapshot head (id, server version) in one store call and never loads an
// inventory blob. Decoding them all per request held over 1 GiB at 500
// clusters with 10 readers (`make bench-server`; #125 SV-14).
func TestFleetDecodesNoInventories(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	st.errs["LatestSnapshot"] = errors.New("inventory blob loaded")
	var f fleetView
	if resp := getJSON(t, ts, "/api/v1/fleet", "", &f); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /fleet = %d, want 200 without loading a snapshot", resp.StatusCode)
	}
	if len(f.Clusters) != 1 || f.Clusters[0].ServerVersion != "v1.34.2" || f.Clusters[0].Cells["1.35"] == nil {
		t.Errorf("fleet = %+v, want prod-eu-1 at v1.34.2 with a 1.35 cell", f)
	}
	// The cluster list's default-target summaries come from the heads too.
	var list []struct {
		Latest *struct {
			Target string `json:"target"`
		} `json:"latest"`
	}
	if resp := getJSON(t, ts, "/api/v1/clusters", "", &list); resp.StatusCode != http.StatusOK || len(list) != 1 || list[0].Latest == nil || list[0].Latest.Target != "1.35" {
		t.Errorf("GET /clusters = %d %+v, want the 1.35 summary without loading a snapshot", resp.StatusCode, list)
	}
}

// TestFleetViewsSurviveCorruptInventory: a cluster whose stored inventory
// does not decode still gets its fleet row, and the teams rollup lists it
// as missing instead of failing.
func TestFleetViewsSurviveCorruptInventory(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	ctx := context.Background()
	id, err := st.UpsertCluster(ctx, store.Cluster{Name: "broken"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertSnapshot(ctx, store.Snapshot{ClusterID: id, Hash: "x", Inventory: []byte(`{`)}); err != nil {
		t.Fatal(err)
	}
	var f fleetView
	if resp := getJSON(t, ts, "/api/v1/fleet", "", &f); resp.StatusCode != http.StatusOK || len(f.Clusters) != 2 {
		t.Fatalf("GET /fleet = %d %+v, want both rows", resp.StatusCode, f)
	}
	var teams struct {
		Missing []string `json:"missing"`
	}
	if resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.35", "", &teams); resp.StatusCode != http.StatusOK || !slices.Contains(teams.Missing, "broken") {
		t.Errorf("GET /fleet/teams = %d %+v, want 200 listing broken as missing", resp.StatusCode, teams)
	}
}
