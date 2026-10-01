package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// fakeClock is a settable clock safe to read from handler goroutines.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// harness is a server on a real SQLite file, so a "restart" (a new Server
// on the same database, maybe with a new KB or config) is just restart().
type harness struct {
	t     *testing.T
	path  string
	st    *store.SQLite
	srv   *Server
	ts    *httptest.Server
	rec   *recordingNotifier
	clock *fakeClock
}

func newHarness(t *testing.T, cfg Config, at time.Time) *harness {
	t.Helper()
	h := &harness{t: t, path: filepath.Join(t.TempDir(), "db.sqlite"), rec: &recordingNotifier{}, clock: &fakeClock{t: at}}
	h.restart(cfg)
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	if h.ts != nil {
		h.ts.Close()
		_ = h.st.Close()
		h.ts = nil
	}
}

// restart opens a new Server (cfg; Store, Notifier and IngestToken are
// filled in) on the same database. It does not run the startup pass: call
// h.tick() for that.
func (h *harness) restart(cfg Config) {
	h.t.Helper()
	h.stop()
	st, err := store.Open(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	cfg.Store, cfg.Notifier, cfg.IngestToken = st, h.rec, "ingest-tok"
	srv, err := New(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	srv.now = h.clock.now
	h.st, h.srv, h.ts = st, srv, httptest.NewServer(srv.Handler())
}

func (h *harness) push(cluster string, inv inventory.Inventory) (int, map[string]any) {
	h.t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "clusterName": cluster, "agentVersion": "test", "kbVersion": "agent-kb", "inventory": inv,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	resp, out := postSnapshot(h.t, h.ts, "ingest-tok", body, false)
	return resp.StatusCode, out
}

// tick runs one re-evaluation pass, as the startup pass and the hourly
// ticker do.
func (h *harness) tick() {
	h.t.Helper()
	h.srv.reevaluateAll(context.Background())
}

// drain delivers every due outbox message, as the worker does after commit.
func (h *harness) drain() []notify.Event {
	h.t.Helper()
	before := len(h.rec.all())
	h.srv.deliverOutbox(context.Background())
	return h.rec.all()[before:]
}

func (h *harness) get(path string, into any) int {
	h.t.Helper()
	return getJSON(h.t, h.ts, path, "", into).StatusCode
}

func (h *harness) clusterID(name string) int64 {
	h.t.Helper()
	cs, err := h.st.ListClusters(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, c := range cs {
		if c.Name == name {
			return c.ID
		}
	}
	h.t.Fatalf("no cluster %q", name)
	return 0
}

type fleetCellView struct {
	Score       int       `json:"score"`
	Ready       bool      `json:"ready"`
	Verdict     string    `json:"verdict"`
	Blockers    int       `json:"blockers"`
	EvaluatedAt time.Time `json:"evaluatedAt"`
	SnapshotID  int64     `json:"snapshotId"`
	Source      string    `json:"source"`
}

type fleetView struct {
	Targets  []string `json:"targets"`
	Clusters []struct {
		Name          string                    `json:"name"`
		ServerVersion string                    `json:"serverVersion"`
		NotApplicable []string                  `json:"notApplicable"`
		Cells         map[string]*fleetCellView `json:"cells"`
	} `json:"clusters"`
}

func (h *harness) fleetCell(cluster, target string) *fleetCellView {
	h.t.Helper()
	var f fleetView
	if code := h.get("/api/v1/fleet?targets="+target, &f); code != http.StatusOK {
		h.t.Fatalf("GET /fleet status %d", code)
	}
	for _, row := range f.Clusters {
		if row.Name == cluster {
			return row.Cells[target]
		}
	}
	h.t.Fatalf("fleet has no row %q", cluster)
	return nil
}

// istioKB is testKB plus one add-on whose EOL date the tests cross.
func istioKB() kb.KB {
	k := testKB()
	k.AddOns = []registry.AddOn{{
		SchemaVersion: 1, ID: "istio", DisplayName: "Istio",
		Support: registry.Support{Status: "supported", EOLDate: "2026-11-30", Citations: []string{"https://istio.io/latest/docs/releases/supported-releases/"}},
	}}
	return k
}

func istioInventory() inventory.Inventory {
	inv := testInventory() // v1.34.2 → default target 1.35
	inv.AddOns = []inventory.AddOnInstance{{ID: "istio", Version: "1.24.0", Namespaces: []string{"istio-system"}, Source: "image"}}
	return inv
}

var (
	aug1  = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	dec15 = time.Date(2026, 12, 15, 9, 0, 0, 0, time.UTC)
)

// TestDuplicatePushReevaluatesAcrossEOLDate is the frozen-verdict repro: an
// identical inventory pushed before and after an add-on's EOL date. The
// duplicate push must re-evaluate (the stored row predates today), update
// the fleet cell and alert exactly once.
func TestDuplicatePushReevaluatesAcrossEOLDate(t *testing.T) {
	h := newHarness(t, Config{KB: istioKB()}, aug1)
	if code, out := h.push("prod", istioInventory()); code != http.StatusAccepted {
		t.Fatalf("first push = %d %v", code, out)
	}
	if c := h.fleetCell("prod", "1.35"); c == nil || c.Score != 100 || !c.Ready || c.Verdict != "ready" {
		t.Fatalf("Aug 1 cell = %+v, want score 100 ready", c)
	}
	h.drain()

	h.clock.set(dec15)
	if code, out := h.push("prod", istioInventory()); code != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("duplicate push = %d %v, want 200 duplicate", code, out)
	}
	c := h.fleetCell("prod", "1.35")
	if c == nil || c.Ready || c.Verdict != "blocked" || c.Blockers != 1 || c.Score >= 100 {
		t.Fatalf("Dec 15 cell = %+v, want blocked with 1 blocker", c)
	}
	if !c.EvaluatedAt.Equal(dec15) || c.Source != "stored" || c.SnapshotID == 0 {
		t.Errorf("cell freshness = %+v, want evaluatedAt %v, source stored, a snapshot id", c, dec15)
	}
	evs := h.drain()
	if len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || !strings.Contains(evs[0].Title, "Istio") {
		t.Fatalf("events = %+v, want exactly one Istio new-blocker", evs)
	}

	// Pushed again the same day: nothing is stale, nothing is written.
	if code, _ := h.push("prod", istioInventory()); code != http.StatusOK {
		t.Fatalf("third push = %d", code)
	}
	hist, err := h.st.ScoreHistory(context.Background(), h.clusterID("prod"), "1.35", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Errorf("history = %+v, want 2 points (ready, then blocked)", hist)
	}
	if evs := h.drain(); len(evs) != 0 {
		t.Errorf("same-day duplicate notified again: %+v", evs)
	}
}

// TestUnchangedReevaluationRefreshesInsteadOfAddingHistory: a re-evaluation
// on a later day with the same verdict, score and findings bumps
// evaluatedAt on the existing row.
func TestUnchangedReevaluationRefreshesInsteadOfAddingHistory(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("prod", testInventoryWithPSP())
	next := aug1.Add(30 * time.Hour)
	h.clock.set(next)
	if code, _ := h.push("prod", testInventoryWithPSP()); code != http.StatusOK {
		t.Fatalf("duplicate push = %d", code)
	}
	cid := h.clusterID("prod")
	hist, _ := h.st.ScoreHistory(context.Background(), cid, "1.35", 0)
	if len(hist) != 1 {
		t.Errorf("history = %d points, want 1 (unchanged result refreshes)", len(hist))
	}
	cur, err := h.st.CurrentEvaluation(context.Background(), cid, "1.35")
	if err != nil {
		t.Fatal(err)
	}
	if !cur.EvaluatedAt.Equal(next) || !cur.CreatedAt.Equal(aug1) {
		t.Errorf("row created %v evaluated %v, want %v / %v", cur.CreatedAt, cur.EvaluatedAt, aug1, next)
	}
	// The audit export dates the refreshed report by its refresh, as
	// /report and /fleet do — not by when the row was first recorded.
	_, csvBody := getExportFor(t, h.ts, cid, "?target=1.35&format=csv")
	if want := "," + next.Format(time.RFC3339) + ","; !strings.Contains(string(csvBody), want) || strings.Contains(string(csvBody), aug1.Format(time.RFC3339)) {
		t.Errorf("CSV export evaluatedAt is not the refresh time %s:\n%s", next.Format(time.RFC3339), csvBody)
	}
	_, htmlBody := getExportFor(t, h.ts, cid, "?target=1.35&format=html")
	if want := "Evaluated: " + next.Format("2006-01-02 15:04 UTC"); !strings.Contains(string(htmlBody), want) {
		t.Errorf("HTML export lacks %q", want)
	}
	if evs := h.drain(); len(evs) != 0 {
		t.Errorf("unchanged re-evaluation notified: %+v", evs)
	}
}

// TestRestartWithNewKBReevaluatesOnDuplicatePush: a server upgrade (new KB
// version) takes effect on the next duplicate push, the same day.
func TestRestartWithNewKBReevaluatesOnDuplicatePush(t *testing.T) {
	oldKB := testKB()
	oldKB.Version = "kb-old"
	oldKB.APILifecycle = nil // the old KB does not know PSP is removed
	h := newHarness(t, Config{KB: oldKB}, aug1)
	h.push("prod", testInventoryWithPSP())
	if c := h.fleetCell("prod", "1.35"); c == nil || !c.Ready {
		t.Fatalf("old-KB cell = %+v, want ready", c)
	}

	newKB := testKB()
	newKB.Version = "kb-new"
	h.restart(Config{KB: newKB})
	if code, _ := h.push("prod", testInventoryWithPSP()); code != http.StatusOK {
		t.Fatalf("duplicate push after restart = %d", code)
	}
	if c := h.fleetCell("prod", "1.35"); c == nil || c.Ready || c.Blockers != 1 {
		t.Fatalf("new-KB cell = %+v, want the PSP blocker", c)
	}
	var rep struct {
		KBVersion string `json:"kbVersion"`
	}
	h.get("/api/v1/clusters/1/report?target=1.35", &rep)
	if rep.KBVersion != "kb-new" {
		t.Errorf("report kbVersion = %q, want kb-new", rep.KBVersion)
	}
}

// TestNewTargetGetsCellAndExport: a target added to --targets gets a fleet
// cell and a working export from the startup pass, and from the next
// duplicate push, without a changed inventory.
func TestNewTargetGetsCellAndExport(t *testing.T) {
	for _, via := range []string{"startup pass", "duplicate push"} {
		t.Run(via, func(t *testing.T) {
			h := newHarness(t, Config{KB: testKB()}, aug1)
			h.push("prod", testInventoryWithPSP())
			h.restart(Config{KB: testKB(), ExtraTargets: []string{"1.36"}})
			if c := h.fleetCell("prod", "1.36"); c != nil {
				t.Fatalf("1.36 cell before any pass = %+v, want null", c)
			}
			if via == "startup pass" {
				h.tick()
			} else if code, _ := h.push("prod", testInventoryWithPSP()); code != http.StatusOK {
				t.Fatalf("duplicate push = %d", code)
			}
			if c := h.fleetCell("prod", "1.36"); c == nil || c.Blockers != 1 {
				t.Fatalf("1.36 cell = %+v, want the PSP blocker", c)
			}
			resp, body := getExport(t, h.ts, "?target=1.36&format=csv")
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "PodSecurityPolicy") {
				t.Fatalf("export 1.36 = %d %s", resp.StatusCode, body)
			}
		})
	}
}

// TestTeamMapChangeReevaluates: an edited --team-map re-attributes stored
// findings on the next duplicate push, the same day.
func TestTeamMapChangeReevaluates(t *testing.T) {
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay-prod", Team: "payments"}}
	inv.APIUsage = []inventory.APIUsage{{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1, Namespaces: map[string]int{"pay-prod": 1}}}
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("prod", inv)

	h.restart(Config{KB: testKB(), TeamMap: TeamMap{{Pattern: "pay-*", Team: "billing"}}})
	if code, _ := h.push("prod", inv); code != http.StatusOK {
		t.Fatalf("duplicate push = %d", code)
	}
	var teams struct {
		Teams map[string]any `json:"teams"`
	}
	h.get("/api/v1/clusters/1/teams?target=1.35", &teams)
	if _, ok := teams.Teams["billing"]; !ok || len(teams.Teams) != 1 {
		t.Fatalf("teams = %v, want only billing after the team-map change", teams.Teams)
	}
}

// TestReadPathsUseLatestSnapshotOnly is the mixed-version fleet repro:
// prod upgrades from 1.34 (with PSP) to 1.35.1 (no PSP). Nothing may keep
// serving the old snapshot's 1.35 PSP blocker, and 1.35 is not applicable
// to a cluster already on it.
func TestReadPathsUseLatestSnapshotOnly(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	staging := testInventory()
	staging.ClusterID = "uid-staging"
	h.push("staging", staging)
	old := testInventoryWithPSP()
	old.Namespaces = []inventory.NamespaceInfo{{Name: "", Team: "payments"}}
	h.push("prod", old)
	upgraded := testInventory()
	upgraded.ServerVersion = "v1.35.1"
	h.push("prod", upgraded)
	prod := h.clusterID("prod")

	var f fleetView
	h.get("/api/v1/fleet?targets=1.35,1.36", &f)
	for _, row := range f.Clusters {
		if row.Name != "prod" {
			continue
		}
		if row.Cells["1.35"] != nil {
			t.Errorf("prod 1.35 cell = %+v, want null (already on 1.35)", row.Cells["1.35"])
		}
		if len(row.NotApplicable) != 1 || row.NotApplicable[0] != "1.35" || row.ServerVersion != "v1.35.1" {
			t.Errorf("prod row n/a = %v version %q, want [1.35] on v1.35.1", row.NotApplicable, row.ServerVersion)
		}
		if c := row.Cells["1.36"]; c == nil || !c.Ready {
			t.Errorf("prod 1.36 cell = %+v, want ready", c)
		}
	}

	var teams struct {
		Teams map[string]any `json:"teams"`
	}
	h.get("/api/v1/fleet/teams?target=1.35", &teams)
	if _, ok := teams.Teams["payments"]; ok {
		t.Errorf("fleet teams 1.35 = %v, still blames payments for prod's old PSP", teams.Teams)
	}

	var rep struct {
		Findings      []struct{ Title string } `json:"findings"`
		NotApplicable bool                     `json:"notApplicable"`
		Source        string                   `json:"source"`
	}
	h.get("/api/v1/clusters/"+itoa(prod)+"/report?target=1.35", &rep)
	if !rep.NotApplicable || rep.Source != "what-if" {
		t.Errorf("report 1.35 = n/a %v source %q, want n/a what-if", rep.NotApplicable, rep.Source)
	}
	for _, f := range rep.Findings {
		if strings.Contains(f.Title, "PodSecurityPolicy") {
			t.Errorf("report 1.35 still lists %q", f.Title)
		}
	}

	resp, body := getExportFor(t, h.ts, prod, "?target=1.35&format=csv")
	if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "PodSecurityPolicy") {
		t.Errorf("export 1.35 still lists the old PSP blocker: %s", body)
	}
}

// TestUnknownVerdictNeverReadyOrAlerting: a transient collector failure
// (api-usage unavailable → verdict unknown) must not alert became-ready,
// must not render a ready cell, and its recovery must not re-alert the
// blocker that never went away.
func TestUnknownVerdictNeverReadyOrAlerting(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("prod", testInventoryWithPSP())

	broken := testInventory()
	broken.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: false, Reason: "forbidden"}
	h.push("prod", broken)
	c := h.fleetCell("prod", "1.35")
	if c == nil || c.Ready || c.Verdict != "unknown" {
		t.Fatalf("cell during collector failure = %+v, want not ready, verdict unknown", c)
	}
	if evs := h.drain(); len(evs) != 0 {
		t.Fatalf("collector failure notified: %+v", evs)
	}

	h.push("prod", testInventoryWithPSP()) // collector back, PSP still there
	if evs := h.drain(); len(evs) != 0 {
		t.Fatalf("recovery re-alerted the existing blocker: %+v", evs)
	}

	h.push("prod", testInventory()) // PSP really removed
	evs := h.drain()
	if len(evs) != 1 || evs[0].Kind != notify.KindBecameReady {
		t.Fatalf("events = %+v, want exactly one became-ready", evs)
	}
}

// TestStartRunsBackgroundPassAndDelivery: Start runs the startup
// re-evaluation pass and the delivery worker without any push, and
// Shutdown stops both. A server upgraded to a KB that knows PSP is removed
// must flag and alert the stored cluster on its own.
func TestStartRunsBackgroundPassAndDelivery(t *testing.T) {
	oldKB := testKB()
	oldKB.Version = "kb-old"
	oldKB.APILifecycle = nil
	h := newHarness(t, Config{KB: oldKB}, aug1)
	h.push("prod", testInventoryWithPSP())
	h.stop()

	st, err := store.Open(h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := New(Config{Listen: "127.0.0.1:0", Store: st, KB: testKB(), Notifier: h.rec, IngestToken: "ingest-tok"})
	if err != nil {
		t.Fatal(err)
	}
	srv.now = h.clock.now
	stop := startServer(t, srv)

	deadline := time.Now().Add(5 * time.Second)
	for len(h.rec.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	evs := h.rec.all()
	if len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || evs[0].Cluster != "prod" {
		t.Fatalf("background events = %+v, want one new-blocker for prod", evs)
	}
	e, err := st.CurrentEvaluation(context.Background(), 1, "1.35")
	if err != nil || e.KBVersion != testKB().Version || e.Blockers != 1 {
		t.Fatalf("current 1.35 = (%+v, %v), want re-evaluated with the new KB", e, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stop(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func getExportFor(t *testing.T, ts *httptest.Server, clusterID int64, query string) (*http.Response, []byte) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + "/api/v1/clusters/" + itoa(clusterID) + "/export" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		t.Fatal(err)
	}
	return resp, []byte(buf.String())
}
