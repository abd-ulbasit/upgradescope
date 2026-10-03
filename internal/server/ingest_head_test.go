package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIngestLoadsNoPreviousInventory: a cluster's later push compares its
// hash with the latest snapshot's head and reads the version it was
// judged at from it, never the stored inventory, which is up to
// --max-snapshot-bytes and which SQLite's driver copies once more.
func TestIngestLoadsNoPreviousInventory(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	st.errs["LatestSnapshot"] = errors.New("inventory blob loaded")

	changed := testInventoryWithPSP()
	changed.APIUsage[0].Count = 3
	for _, push := range []struct {
		name string
		body []byte
		want int
	}{
		{"a new snapshot", pushReqBody(t, changed), http.StatusAccepted},
		{"the same again", pushReqBody(t, changed), http.StatusOK},
		{"a degraded one", pushReqBody(t, degraded(changed)), http.StatusAccepted},
	} {
		if resp, out := postSnapshot(t, ts, "ingest-tok", push.body, true); resp.StatusCode != push.want {
			t.Errorf("%s: status = %d (%v), want %d without loading a stored inventory", push.name, resp.StatusCode, out, push.want)
		}
	}
}
