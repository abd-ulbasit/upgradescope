package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// pushEnvelope wraps a raw inventory JSON in a schemaVersion 1 push for
// cluster prod-eu-1.
func pushEnvelope(t *testing.T, inventory string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"clusterName":   "prod-eu-1",
		"agentVersion":  "v0.2.0-test",
		"kbVersion":     "agent-kb",
		"inventory":     json.RawMessage(inventory),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIngestCapabilityLessInventoryNeverReady (#194 SV-04, NEW-ingest-1):
// a push whose inventory is schema-valid but reports no capabilities, or
// an empty map, was accepted and judged on what it did not contain:
// blocked prod (a PodSecurityPolicy, removed in 1.35) read ready/100,
// only crds not assessed. Such a push is still stored (it is valid under
// the published schema), and judged unknown: the required capabilities
// it does not report are required gaps.
func TestIngestCapabilityLessInventoryNeverReady(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	push := func(cluster, agentVersion, inventory string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"schemaVersion": 1, "clusterName": cluster, "agentVersion": agentVersion, "kbVersion": "x",
			"inventory": json.RawMessage(inventory),
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp, out := postSnapshot(t, h.ts, "ingest-tok", body, false); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s push as %q = %d %v", cluster, agentVersion, resp.StatusCode, out)
		}
	}
	// "x" is the finding's repro (an unmarked inventory, so also legacy);
	// 0.2.0-rc.2 is judged as current, so only the missing capabilities
	// keep it from ready.
	for i, agent := range []string{"x", "0.2.0-rc.2"} {
		prod := "prod-" + itoa(int64(i))
		if code, out := h.pushAs(prod, "v0.2.0", testInventoryWithPSP()); code != http.StatusAccepted {
			t.Fatalf("seed push = %d %v", code, out)
		}
		if c := h.fleetCell(prod, "1.35"); c == nil || c.Verdict != "blocked" {
			t.Fatalf("seed: cell = %+v, want blocked", c)
		}
		push(prod, agent, `{"schemaVersion":1,"serverVersion":"v1.34.2","clusterId":"uid-123"}`)
		if c := h.fleetCell(prod, "1.35"); c == nil || c.Ready || c.Verdict != "unknown" {
			t.Errorf("agent %q, no capabilities: cell = %+v, want unknown, not ready", agent, c)
		}
		if reason, required := h.report(prod, "1.35").gap("api-usage"); !required {
			t.Errorf("agent %q, no capabilities: api-usage gap %q not required", agent, reason)
		}

		clean := "clean-" + itoa(int64(i))
		push(clean, agent, `{"schemaVersion":1,"serverVersion":"v1.34.2","clusterId":"uid-`+clean+`","capabilities":{}}`)
		rep := h.report(clean, "1.35")
		if rep.Verdict != "unknown" {
			t.Errorf("agent %q, capabilities {}: verdict %s, want unknown", agent, rep.Verdict)
		}
		for _, c := range []string{"api-usage", "versions"} {
			if _, required := rep.gap(c); !required {
				t.Errorf("agent %q, capabilities {}: %s not a required gap (%+v)", agent, c, rep.NotAssessed)
			}
		}
	}
}

// TestIngestRejectsMalformedInventory: an inventory the server cannot
// judge is refused with 422 before anything is written. Accepting one
// made it the cluster's latest snapshot, which blanked a blocked
// cluster's fleet cell, report and team rollup, and bumped last seen.
func TestIngestRejectsMalformedInventory(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	clock := &fakeClock{t: s.now()}
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
	seenAt := st.clusters[1].LastSeen
	snaps, evals := len(st.snapshots), len(st.evals)
	clock.set(clock.now().Add(time.Hour))

	inv := func(mut func(map[string]any)) string {
		raw, _ := json.Marshal(testInventoryWithPSP())
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mut(m)
		out, _ := json.Marshal(m)
		return string(out)
	}
	cases := map[string]string{
		"null inventory":        `null`,
		"empty inventory":       `{}`,
		"schemaVersion 2":       inv(func(m map[string]any) { m["schemaVersion"] = 2 }),
		"schemaVersion 99":      inv(func(m map[string]any) { m["schemaVersion"] = 99 }),
		"schemaVersion missing": inv(func(m map[string]any) { delete(m, "schemaVersion") }),
		"garbage serverVersion": inv(func(m map[string]any) { m["serverVersion"] = "garbage" }),
		"major 2 serverVersion": inv(func(m map[string]any) { m["serverVersion"] = "v2.0.0" }),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp, out := postSnapshot(t, ts, "ingest-tok", pushEnvelope(t, body), false)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d (body %v), want 422", resp.StatusCode, out)
			}
			if len(st.snapshots) != snaps || len(st.evals) != evals {
				t.Errorf("rows changed: snapshots %d -> %d, evaluations %d -> %d", snaps, len(st.snapshots), evals, len(st.evals))
			}
			if got := st.clusters[1].LastSeen; !got.Equal(seenAt) {
				t.Errorf("last seen moved %v -> %v", seenAt, got)
			}
		})
	}
	// A new cluster name with a malformed inventory registers nothing.
	body := pushEnvelope(t, `{}`)
	body = []byte(string(body[:len(body)-1]) + `,"clusterName":"other"}`)
	if resp, _ := postSnapshot(t, ts, "ingest-tok", body, false); resp.StatusCode != http.StatusUnprocessableEntity || len(st.clusters) != 1 {
		t.Errorf("malformed push for a new name = %d, clusters %d, want 422 and no new cluster", resp.StatusCode, len(st.clusters))
	}
}

// TestIngestStoreFailureLeavesNoClusterRow: a push whose commit fails is
// a 500 that leaves nothing behind — no cluster row for a new name, no
// last-seen bump for a known one.
func TestIngestStoreFailureLeavesNoClusterRow(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	st.errs["CommitEvaluations"] = errors.New("disk full")
	if resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), true); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if len(st.clusters) != 0 {
		t.Errorf("failed push left clusters %+v, want none", st.clusters)
	}
}

// TestDuplicatePushRecordsAgentVersion: an agent upgrade that leaves the
// inventory unchanged is a duplicate push, and the latest snapshot now
// says which agent and KB pushed it last.
func TestDuplicatePushRecordsAgentVersion(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first push = %d", resp.StatusCode)
	}
	raw, _ := json.Marshal(testInventory())
	var req map[string]any
	_ = json.Unmarshal(pushEnvelope(t, string(raw)), &req)
	req["agentVersion"], req["kbVersion"] = "v0.3.0", "kb-2026-10-01"
	body, _ := json.Marshal(req)
	resp, out := postSnapshot(t, ts, "ingest-tok", body, false)
	if resp.StatusCode != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("second push = %d %v, want 200 duplicate", resp.StatusCode, out)
	}
	if len(st.snapshots) != 1 || st.snapshots[0].AgentVersion != "v0.3.0" || st.snapshots[0].KBVersion != "kb-2026-10-01" {
		t.Errorf("snapshots = %+v, want one recording agent v0.3.0 and kb-2026-10-01", st.snapshots)
	}
}
