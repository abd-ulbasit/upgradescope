package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// newTestServer builds a Server on a fake store with a pinned clock.
func newTestServer(t *testing.T, st *fakeStore, opts ...func(*Config)) *Server {
	t.Helper()
	cfg := Config{Store: st, KB: testKB(), IngestToken: "ingest-tok"}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	return s
}

func testInventory() inventory.Inventory {
	return inventory.Inventory{
		SchemaVersion: 1,
		ClusterID:     "uid-123",
		CollectedAt:   time.Date(2026, 6, 10, 11, 0, 0, 0, time.UTC),
		ServerVersion: "v1.34.2",
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapVersions: {Available: true},
		},
	}
}

// testInventoryWithPSP adds one PodSecurityPolicy residency: with testKB
// (PSP removed in 1.35) this yields exactly 1 blocker for targets >= 1.35
// (score 75, not ready) and 1 warning for target 1.34.
func testInventoryWithPSP() inventory.Inventory {
	inv := testInventory()
	inv.APIUsage = []inventory.APIUsage{{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
		Count: 2, Namespaces: map[string]int{"": 2},
	}}
	return inv
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func pushReqBody(t *testing.T, inv inventory.Inventory) []byte {
	t.Helper()
	invJSON, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	b, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"clusterName":   "prod-eu-1",
		"agentVersion":  "v0.2.0-test",
		"kbVersion":     "agent-kb",
		"inventory":     json.RawMessage(invJSON),
	})
	if err != nil {
		t.Fatalf("marshal push request: %v", err)
	}
	return b
}

// postSnapshot POSTs body (gzipped iff gzipped) and decodes the JSON reply.
func postSnapshot(t *testing.T, ts *httptest.Server, token string, body []byte, gzipped bool) (*http.Response, map[string]any) {
	t.Helper()
	payload := body
	if gzipped {
		payload = gzipBytes(t, body)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/snapshots", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/snapshots: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("non-JSON response (status %d): %s", resp.StatusCode, raw)
		}
	}
	return resp, out
}

func TestIngestAuth(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	body := pushReqBody(t, testInventory())
	for _, tc := range []struct{ name, token string }{
		{"missing token", ""},
		{"wrong token", "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, out := postSnapshot(t, ts, tc.token, body, true)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if msg, _ := out["error"].(string); msg == "" {
				t.Fatalf("want JSON error body, got %v", out)
			}
		})
	}
}

func TestIngestAcceptedGzipAndIdentity(t *testing.T) {
	for _, gzipped := range []bool{true, false} {
		name := "identity"
		if gzipped {
			name = "gzip"
		}
		t.Run(name, func(t *testing.T) {
			st := newFakeStore()
			ts := httptest.NewServer(newTestServer(t, st).Handler())
			defer ts.Close()
			resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), gzipped)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status = %d (body %v), want 202", resp.StatusCode, out)
			}
			if _, ok := out["snapshotId"].(float64); !ok {
				t.Fatalf("response %v missing numeric snapshotId", out)
			}
			if d, ok := out["duplicate"]; ok && d == true {
				t.Fatalf("first push flagged duplicate: %v", out)
			}
			if len(st.snapshots) != 1 {
				t.Fatalf("stored snapshots = %d, want 1", len(st.snapshots))
			}
			sn := st.snapshots[0]
			if sn.Hash == "" || sn.KBVersion != "agent-kb" || sn.AgentVersion != "v0.2.0-test" {
				t.Fatalf("snapshot fields = %+v", sn)
			}
			var stored inventory.Inventory
			if err := json.Unmarshal(sn.Inventory, &stored); err != nil {
				t.Fatalf("stored inventory is not canonical JSON: %v", err)
			}
			if stored.ClusterID != "uid-123" {
				t.Fatalf("stored ClusterID = %q", stored.ClusterID)
			}
			if len(st.clusters) != 1 {
				t.Fatalf("clusters = %d, want 1", len(st.clusters))
			}
			c := st.clusters[1]
			if c.Name != "prod-eu-1" || c.ClusterUID != "uid-123" {
				t.Fatalf("cluster = %+v", c)
			}
		})
	}
}

func TestIngestDuplicateCanonicalHash(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()

	resp1, out1 := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), true)
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first push status = %d, want 202", resp1.StatusCode)
	}
	evalsAfterFirst := len(st.evals)

	// Same logical inventory, different wire key order and encoding: the
	// canonical re-marshal must produce the same hash → duplicate.
	reordered := []byte(`{
	  "schemaVersion": 1,
	  "clusterName": "prod-eu-1",
	  "agentVersion": "v0.2.0-test",
	  "kbVersion": "agent-kb",
	  "inventory": {
	    "capabilities": {"versions": {"available": true}},
	    "serverVersion": "v1.34.2",
	    "collectedAt": "2026-06-10T11:00:00Z",
	    "clusterId": "uid-123",
	    "schemaVersion": 1
	  }
	}`)
	resp2, out2 := postSnapshot(t, ts, "ingest-tok", reordered, false)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("duplicate push status = %d (body %v), want 200", resp2.StatusCode, out2)
	}
	if out2["duplicate"] != true {
		t.Fatalf("duplicate push body = %v, want duplicate:true", out2)
	}
	if out1["snapshotId"] != out2["snapshotId"] {
		t.Fatalf("snapshotId mismatch: %v vs %v", out1["snapshotId"], out2["snapshotId"])
	}
	if len(st.snapshots) != 1 {
		t.Fatalf("snapshots after duplicate = %d, want 1", len(st.snapshots))
	}
	if len(st.evals) != evalsAfterFirst {
		t.Fatalf("duplicate triggered re-evaluation: evals %d -> %d", evalsAfterFirst, len(st.evals))
	}
}

func TestIngestValidation(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	cases := []struct {
		name string
		body string
	}{
		{"schemaVersion 2", `{"schemaVersion":2,"clusterName":"c","inventory":{}}`},
		{"schemaVersion missing", `{"clusterName":"c","inventory":{}}`},
		{"clusterName missing", `{"schemaVersion":1,"inventory":{}}`},
		{"inventory missing", `{"schemaVersion":1,"clusterName":"c"}`},
		{"inventory wrong type", `{"schemaVersion":1,"clusterName":"c","inventory":42}`},
		{"malformed JSON", `{"schemaVersion":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, out := postSnapshot(t, ts, "ingest-tok", []byte(tc.body), false)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d (body %v), want 422", resp.StatusCode, out)
			}
			if msg, _ := out["error"].(string); msg == "" {
				t.Fatalf("want structured {\"error\":...}, got %v", out)
			}
		})
	}
}

func TestIngestEncodingErrors(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()

	t.Run("unsupported encoding", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/snapshots",
			bytes.NewReader(pushReqBody(t, testInventory())))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer ingest-tok")
		req.Header.Set("Content-Encoding", "br")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", resp.StatusCode)
		}
	})

	t.Run("bad gzip", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/snapshots",
			bytes.NewReader([]byte("definitely not gzip")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer ingest-tok")
		req.Header.Set("Content-Encoding", "gzip")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", resp.StatusCode)
		}
	})
}

func TestIngestBodyLimits(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()

	t.Run("identity over 20MiB", func(t *testing.T) {
		huge := bytes.Repeat([]byte("a"), maxSnapshotBody+1)
		resp, _ := postSnapshot(t, ts, "ingest-tok", huge, false)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("gzip bomb", func(t *testing.T) {
		// Tiny on the wire, >20MiB decompressed: the post-decompression cap
		// must fire.
		bomb := gzipBytes(t, make([]byte, maxSnapshotBody+2))
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/snapshots", bytes.NewReader(bomb))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer ingest-tok")
		req.Header.Set("Content-Encoding", "gzip")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})
}

func TestIngestEvaluatesDefaultAndExtraTargets(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.ExtraTargets = []string{"1.37"} })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d (body %v), want 202", resp.StatusCode, out)
	}
	// Default target = next minor above v1.34.2 → 1.35, plus extra 1.37.
	if len(st.evals) != 2 {
		t.Fatalf("evaluations = %d, want 2 (default 1.35 + extra 1.37)", len(st.evals))
	}
	byTarget := map[string]store.Evaluation{}
	for _, e := range st.evals {
		byTarget[e.Target] = e
	}
	e135, ok := byTarget["1.35"]
	if !ok {
		t.Fatalf("no evaluation for default target 1.35; got %v", byTarget)
	}
	if e135.Score != 75 || e135.Ready || e135.Blockers != 1 || e135.Warnings != 0 {
		t.Fatalf("1.35 eval = score %d ready %v blockers %d warnings %d, want 75 false 1 0",
			e135.Score, e135.Ready, e135.Blockers, e135.Warnings)
	}
	if e135.KBVersion != "test-kb" {
		t.Fatalf("eval KBVersion = %q, want server KB version test-kb", e135.KBVersion)
	}
	if e135.SnapshotID == 0 || e135.ClusterID == 0 {
		t.Fatalf("eval missing FK linkage: %+v", e135)
	}
	var rep engine.Report
	if err := json.Unmarshal(e135.Report, &rep); err != nil {
		t.Fatalf("stored report is not engine.Report JSON: %v", err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Severity != engine.SevBlocker ||
		rep.Findings[0].Category != engine.CatRemovedAPI {
		t.Fatalf("report findings = %+v, want 1 removed-api blocker", rep.Findings)
	}
	if _, ok := byTarget["1.37"]; !ok {
		t.Fatal("no evaluation for extra target 1.37")
	}
}

func TestIngestDedupsExtraTargetEqualToDefault(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.ExtraTargets = []string{"1.35"} })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if len(st.evals) != 1 {
		t.Fatalf("evaluations = %d, want 1 (extra target equals default)", len(st.evals))
	}
}

func TestIngestSkipsDefaultTargetWhenServerVersionUnparseable(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	inv := testInventory()
	inv.ServerVersion = "" // versions capability degraded
	resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (snapshot still accepted)", resp.StatusCode)
	}
	if len(st.evals) != 0 {
		t.Fatalf("evaluations = %d, want 0 (no parseable default, no extras)", len(st.evals))
	}
	if len(st.snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(st.snapshots))
	}
}
