package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recordingNotifier) Notify(_ context.Context, ev notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingNotifier) all() []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Event(nil), r.events...)
}

// pushInventory pushes one inventory through the real ingest endpoint
// (gzip JSON + bearer, per the snapshot push protocol) and requires 202.
func pushInventory(t *testing.T, baseURL, token string, inv inventory.Inventory) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"clusterName":   "prod-test",
		"agentVersion":  "test",
		"kbVersion":     "test",
		"inventory":     inv,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/snapshots", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest status = %d, want 202", resp.StatusCode)
	}
}

// TestIngestEmitsDeltaNotifications drives two snapshots end-to-end:
// snapshot 1 carries PodSecurityPolicy usage (removed 1.25 → blocker for
// target 1.36) and must NOT notify (first-ever evaluation); snapshot 2 has
// the usage gone (blockers 1→0) and must emit exactly one became-ready
// event carrying the envelope cluster name. The real SQLite store makes
// this fail if prev is loaded after InsertEvaluation.
func TestIngestEmitsDeltaNotifications(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kbData, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingNotifier{}
	srv, err := New(Config{Store: st, KB: kbData, Notifier: rec, IngestToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	base := inventory.Inventory{
		SchemaVersion: 1,
		ClusterID:     "uid-1",
		CollectedAt:   time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		ServerVersion: "v1.35.0", // default target = next minor = 1.36
		Capabilities:  map[inventory.Capability]inventory.CapabilityStatus{},
	}

	withPSP := base
	withPSP.APIUsage = []inventory.APIUsage{
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1},
	}
	pushInventory(t, ts.URL, "tok", withPSP)
	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("first evaluation must not notify, got %+v", evs)
	}

	clean := base // content differs from withPSP (no PSP usage) => new hash
	pushInventory(t, ts.URL, "tok", clean)

	evs := rec.all()
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 became-ready event, got %d: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Kind != notify.KindBecameReady {
		t.Errorf("Kind = %q, want %q", ev.Kind, notify.KindBecameReady)
	}
	if ev.Cluster != "prod-test" {
		t.Errorf("Cluster = %q, want envelope clusterName %q (not inventory UID)", ev.Cluster, "prod-test")
	}
	if ev.Target != "1.36" {
		t.Errorf("Target = %q, want 1.36", ev.Target)
	}
}
