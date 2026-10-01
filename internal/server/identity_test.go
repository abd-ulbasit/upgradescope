package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// TestIngestRejectsClusterUIDChange pushes two clusters that share one name
// alternately, the way two agents with the same --cluster-name would. The
// first registers the name; every push carrying the other clusterId is a
// 409 that writes nothing and tells the operator how to resolve it, so the
// two never interleave in one history.
func TestIngestRejectsClusterUIDChange(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := New(Config{Store: st, KB: testKB(), IngestToken: "ingest-tok"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for i := range 4 {
		inv := testInventory()
		inv.ServerVersion = fmt.Sprintf("v1.34.%d", i) // a new hash every push
		want := http.StatusAccepted
		if i%2 == 1 {
			inv.ClusterID = "uid-other"
			want = http.StatusConflict
		}
		resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), true)
		if resp.StatusCode != want {
			t.Fatalf("push %d (clusterId %s): status = %d (body %v), want %d", i, inv.ClusterID, resp.StatusCode, out, want)
		}
		if want != http.StatusConflict {
			continue
		}
		msg, _ := out["error"].(string)
		for _, hint := range []string{`"prod-eu-1"`, "uid-123", "uid-other", "--cluster-name", "upgradescope clusters delete prod-eu-1"} {
			if !strings.Contains(msg, hint) {
				t.Errorf("409 message %q does not mention %s", msg, hint)
			}
		}
	}

	clusters, err := st.ListClusters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || clusters[0].ClusterUID != "uid-123" {
		t.Fatalf("clusters = %+v, want one cluster still bound to uid-123", clusters)
	}
	snap, err := st.LatestSnapshot(context.Background(), clusters[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(snap.Inventory, &inv); err != nil {
		t.Fatal(err)
	}
	if inv.ClusterID != "uid-123" || inv.ServerVersion != "v1.34.2" {
		t.Fatalf("latest snapshot = %s %s, want uid-123's last push (v1.34.2)", inv.ClusterID, inv.ServerVersion)
	}
}
