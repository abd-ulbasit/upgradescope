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
	mu  sync.Mutex
	got []notify.Notification
}

func (r *recordingNotifier) Notify(_ context.Context, n notify.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, n)
	return nil
}

func (r *recordingNotifier) notifications() []notify.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Notification(nil), r.got...)
}

// event is one change for one target of a delivered notification: the
// shape most tests assert on.
type event struct {
	Cluster, Target, Kind, Title, Detail string
}

// all flattens every delivered notification into events, one per change
// and target.
func (r *recordingNotifier) all() []event {
	var out []event
	for _, n := range r.notifications() {
		for _, c := range n.Changes {
			for _, target := range c.Targets {
				out = append(out, event{Cluster: n.Cluster.Name, Target: target, Kind: c.Kind, Title: c.Title, Detail: c.Detail})
			}
		}
	}
	return out
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

// TestIngestSurvivesCorruptStoredReport covers the corrupt-baseline path in
// outboxFor: when the notification baseline's Report blob is not
// valid JSON, the delta is skipped (logged server-side) but ingestion
// still succeeds — the push returns 202 and zero events are delivered.
// Without the corrupt row this exact sequence emits one became-ready
// event (see TestIngestEmitsDeltaNotifications), so zero events proves
// the corrupt branch ran.
func TestIngestSurvivesCorruptStoredReport(t *testing.T) {
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
		SchemaVersion:   1,
		ClusterID:       "uid-1",
		CollectorSchema: inventory.CurrentCollectorSchema,
		CollectedAt:     time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		ServerVersion:   "v1.35.0", // default target = next minor = 1.36
		Capabilities:    collectedCaps(),
	}
	withPSP := base
	withPSP.APIUsage = []inventory.APIUsage{
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1},
	}
	pushInventory(t, ts.URL, "tok", withPSP)
	srv.deliverOutbox(context.Background())

	// Plant a corrupt evaluation as the LATEST row for (cluster, 1.36):
	// the next push's notification baseline will load it and fail to decode.
	ctx := context.Background()
	clusters, err := st.ListClusters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster after first push, got %d", len(clusters))
	}
	snap, err := st.LatestSnapshot(ctx, clusters[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertEvaluation(ctx, store.Evaluation{
		ClusterID:  clusters[0].ID,
		SnapshotID: snap.ID,
		Target:     "1.36",
		Report:     []byte("{not json"),
		Blockers:   1, // a decided verdict, so it is the notification baseline
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	clean := base                          // PSP gone: would emit became-ready if prev were readable
	pushInventory(t, ts.URL, "tok", clean) // asserts the 202 itself
	srv.deliverOutbox(ctx)

	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("corrupt previous report must suppress delta events, got %+v", evs)
	}
}

// TestIngestEmitsDeltaNotifications drives two snapshots end-to-end:
// snapshot 1 carries PodSecurityPolicy usage (removed 1.25 → blocker for
// target 1.36) and must NOT notify (first-ever evaluation); snapshot 2 has
// the usage gone (blockers 1→0) and must emit exactly one became-ready
// event carrying the envelope cluster name. The real SQLite store makes
// this fail if the baseline is loaded after the new evaluation is written.
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
		SchemaVersion:   1,
		ClusterID:       "uid-1",
		CollectorSchema: inventory.CurrentCollectorSchema,
		CollectedAt:     time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		ServerVersion:   "v1.35.0", // default target = next minor = 1.36
		Capabilities:    collectedCaps(),
	}

	withPSP := base
	withPSP.APIUsage = []inventory.APIUsage{
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1},
	}
	pushInventory(t, ts.URL, "tok", withPSP)
	srv.deliverOutbox(context.Background())
	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("first evaluation must not notify, got %+v", evs)
	}

	clean := base // content differs from withPSP (no PSP usage) => new hash
	pushInventory(t, ts.URL, "tok", clean)
	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("ingest delivered inline, want delivery after commit by the worker: %+v", evs)
	}
	srv.deliverOutbox(context.Background())

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
