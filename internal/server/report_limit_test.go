package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// releasesInventory has n Helm releases whose chart kubeVersion excludes
// every target: one blocker each, of ~700 bytes, from ~130 bytes of push.
func releasesInventory(n int) inventory.Inventory {
	inv := testInventory()
	for i := range n {
		inv.HelmReleases = append(inv.HelmReleases, inventory.HelmRelease{Name: fmt.Sprint("r", i), Namespace: "web",
			ChartName: "c", ChartVersion: "1", KubeVersion: "<1.0", Status: "deployed"})
	}
	return inv
}

// limitServer is a server whose reports, like its pushes, are at most
// 64 KiB.
func limitServer(t *testing.T, st *fakeStore) *Server {
	return newTestServer(t, st, func(c *Config) { c.MaxSnapshotBytes = 64 << 10 })
}

// A push whose report for a target would be over --max-snapshot-bytes is
// 413 and stores nothing: a 5 MB push of such releases built three 41 MB
// reports.
func TestIngestRefusesAReportOverTheLimit(t *testing.T) {
	st := newFakeStore()
	s := limitServer(t, st)
	body := pushReqBody(t, releasesInventory(300))
	if len(body) >= 64<<10 {
		t.Fatalf("fixture push is %d bytes, want it under the 64 KiB limit", len(body))
	}
	rec := httptest.NewRecorder()
	serveIngest(s, rec, body, false)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "would be over the 65536 bytes limit for a report") {
		t.Fatalf("status %d (%.300s), want 413 naming the report limit", rec.Code, rec.Body)
	}
	if clusters, err := st.ListClusters(context.Background()); err != nil || len(clusters) != 0 || len(st.snapshots) != 0 {
		t.Fatalf("%d clusters, %d snapshots stored (%v), want none", len(clusters), len(st.snapshots), err)
	}
	rec = httptest.NewRecorder()
	serveIngest(s, rec, pushReqBody(t, releasesInventory(10)), false)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("10 releases: status %d (%.300s), want 202", rec.Code, rec.Body)
	}
}

// Reads that evaluate (a what-if report, the fleet teams rollup, /gate
// with ?cluster=) are bounded the same way; the re-evaluation pass keeps
// what is stored. The snapshot here was stored under a larger limit, as
// one stored before the limit or judged by a knowledge base that flags
// less would be.
func TestEvaluatingReadsRefuseAReportOverTheLimit(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	rec := httptest.NewRecorder()
	serveIngest(s, rec, pushReqBody(t, releasesInventory(300)), false)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("push: status %d (%.300s)", rec.Code, rec.Body)
	}
	stored, err := st.CurrentEvaluation(context.Background(), 1, "1.35")
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.MaxSnapshotBytes = 64 << 10

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get("/api/v1/clusters/1/report?target=1.40"); rec.Code != http.StatusRequestEntityTooLarge ||
		!strings.Contains(rec.Body.String(), "what-if: the report for target 1.40 would be over") {
		t.Errorf("what-if: status %d (%.300s), want 413", rec.Code, rec.Body)
	}
	if rec := get("/api/v1/clusters/1/report?target=1.35"); rec.Code != http.StatusOK {
		t.Errorf("stored report: status %d (%.300s), want 200", rec.Code, rec.Body)
	}
	rec = get("/api/v1/fleet/teams?target=1.40")
	var teams struct {
		Missing  []string        `json:"missing"`
		Excluded []fleetExcluded `json:"excluded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &teams); rec.Code != http.StatusOK || err != nil || len(teams.Missing) != 1 {
		t.Errorf("fleet teams: status %d (%.300s), want 200 with the cluster missing", rec.Code, rec.Body)
	}
	// It has snapshots: it is left out for its size, not for want of one (#243).
	if len(teams.Excluded) != 1 || teams.Excluded[0].Reason != excludedTooLarge {
		t.Errorf("fleet teams excluded = %+v, want the cluster with reason %q", teams.Excluded, excludedTooLarge)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gate?target=1.35&cluster=prod-eu-1",
		strings.NewReader("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n"))
	req.Header.Set("Content-Type", "application/x-yaml")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "would be over") {
		t.Errorf("gate ?cluster=: status %d (%.300s), want 413", rec.Code, rec.Body)
	}

	next := s.now().Add(24 * time.Hour) // every evaluation is outdated
	s.now = func() time.Time { return next }
	s.reevaluateAll(context.Background())
	after, err := st.CurrentEvaluation(context.Background(), 1, "1.35")
	if err != nil || after.ID != stored.ID || !after.EvaluatedAt.Equal(stored.EvaluatedAt) {
		t.Fatalf("after the pass: evaluation %d at %v (%v), want %d at %v kept", after.ID, after.EvaluatedAt, err, stored.ID, stored.EvaluatedAt)
	}
}

// managersInventory has n unknown-api usages of the policy group, each
// with one object whose manager is 128 apostrophes: HTML writes each as
// five bytes, in the finding's detail.
func managersInventory(n int) inventory.Inventory {
	inv := testInventory()
	for i := range n {
		inv.APIUsage = append(inv.APIUsage, inventory.APIUsage{Group: "policy", Version: "v1beta1", Kind: fmt.Sprint("Kind", i), Count: 1,
			Objects: []inventory.ObjectRef{{Name: fmt.Sprint("o", i), Manager: strings.Repeat("'", inventory.MaxManagerBytes)}}})
	}
	return inv
}

// An export is at most --max-snapshot-bytes too: HTML writes ' " & as
// five bytes and < > as four, so the export of a report within its limit
// can be over it. Such an export is 413, saying to read the JSON report;
// one within the limit is answered, and so is the next read.
func TestExportOverTheLimitIs413(t *testing.T) {
	s := limitServer(t, newFakeStore())
	rec := httptest.NewRecorder()
	serveIngest(s, rec, pushReqBody(t, managersInventory(55)), false)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("push: status %d (%.300s)", rec.Code, rec.Body)
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	rec = get("/api/v1/clusters/1/export?target=1.35&format=html")
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "/report") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("html: status %d, %d bytes, Content-Type %q (%.30s), want a 413 JSON error naming the report", rec.Code, rec.Body.Len(),
			rec.Header().Get("Content-Type"), rec.Body)
	}
	if rec := get("/api/v1/clusters/1/export?target=1.35&format=csv"); rec.Code != http.StatusOK || rec.Body.Len() > 64<<10 {
		t.Fatalf("csv: status %d, %d bytes, want 200 within the limit", rec.Code, rec.Body.Len())
	}
	if rec := get("/api/v1/clusters/1/report?target=1.35"); rec.Code != http.StatusOK {
		t.Fatalf("report after the 413: status %d", rec.Code)
	}
}
