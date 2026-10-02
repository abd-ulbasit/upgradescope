package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestSnapshotRoundTripsUnknownFields: the stored snapshot is the
// inventory as the agent sent it — collectedAt and fields this server
// does not know survive — while dedup still ignores formatting and
// collectedAt. A lossy re-marshal used to drop both.
func TestSnapshotRoundTripsUnknownFields(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	raw := `{"schemaVersion":1,"clusterId":"uid-123","collectedAt":"2026-08-01T08:59:00Z",` +
		`"serverVersion":"v1.34.2","capabilities":{"versions":{"available":true}},` +
		`"collectorVersion":"2","futureField":{"z":1,"a":[true]}}`
	push := func(inv string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{
			"schemaVersion": 1, "clusterName": "prod", "agentVersion": "test", "kbVersion": "agent-kb", "inventory": json.RawMessage(inv),
		})
		resp, out := postSnapshot(t, h.ts, "ingest-tok", body, false)
		return resp.StatusCode, out
	}
	if code, out := push(raw); code != http.StatusAccepted {
		t.Fatalf("push = %d %v", code, out)
	}
	snap, err := h.st.LatestSnapshot(context.Background(), h.clusterID("prod"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snap.Inventory, []byte(raw)) {
		t.Errorf("stored inventory = %s, want the pushed bytes %s", snap.Inventory, raw)
	}
	if snap.ServerVersion != "v1.34.2" {
		t.Errorf("stored server version = %q, want v1.34.2", snap.ServerVersion)
	}

	// Reformatted, with a later collectedAt: the same inventory.
	var v map[string]any
	_ = json.Unmarshal([]byte(raw), &v)
	v["collectedAt"] = "2026-08-01T09:09:00Z"
	again, _ := json.MarshalIndent(v, "", "  ")
	if code, out := push(string(again)); code != http.StatusOK || out["duplicate"] != true {
		t.Errorf("reformatted push = %d %v, want 200 duplicate", code, out)
	}
}
