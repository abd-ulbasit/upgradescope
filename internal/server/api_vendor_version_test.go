package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Managed clusters report vendor-suffixed GitVersions; with empty
// server.targets (the chart default) the default target derived from them
// is the only evaluation the server stores, so it must not be skipped.
func TestIngestDefaultTargetFromVendorServerVersion(t *testing.T) {
	for _, sv := range []string{"v1.34.2-gke.1080000", "v1.34.2-eks-aeac579", "v1.34.2+k3s1"} {
		t.Run(sv, func(t *testing.T) {
			st := newFakeStore()
			ts := httptest.NewServer(newTestServer(t, st).Handler())
			defer ts.Close()
			inv := testInventory()
			inv.ServerVersion = sv
			if resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), true); resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", resp.StatusCode)
			}
			if len(st.evals) != 1 || st.evals[0].Target != "1.35" {
				t.Fatalf("evaluations = %+v, want exactly one for default target 1.35", st.evals)
			}
		})
	}
}
