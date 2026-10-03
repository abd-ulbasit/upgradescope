package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestServerRejectsNonV1Targets: Kubernetes has no 2.x. A 2.0 target used
// to be evaluated and stored (a blocked verdict with a skew blocker), and
// the read API answered it with 200, while `scan` rejected it.
func TestServerRejectsNonV1Targets(t *testing.T) {
	if _, err := New(Config{Store: newFakeStore(), KB: testKB(), ExtraTargets: []string{"2.0"}}); err == nil {
		t.Error("New with extra target 2.0 succeeded, want an error")
	}
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	for _, path := range []string{
		"/api/v1/clusters/1/report?target=2.0",
		"/api/v1/clusters/1/findings?target=2.0",
		"/api/v1/clusters/1/history?target=2.0",
		"/api/v1/fleet?targets=1.36,2.0",
		"/api/v1/fleet/teams?target=2.0",
	} {
		if resp := getJSON(t, ts, path, "", nil); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("GET %s = %d, want 422", path, resp.StatusCode)
		}
	}
}

// TestComposedHandlerBareAPIIsJSON404: the bare /api path belongs to the
// API, not the dashboard: a JSON 404, never the SPA's index.html with 200.
func TestComposedHandlerBareAPIIsJSON404(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	var body map[string]string
	resp := getJSON(t, ts, "/api", "", &body)
	if resp.StatusCode != http.StatusNotFound || body["error"] == "" {
		t.Errorf("GET /api = %d %v, want a JSON 404", resp.StatusCode, body)
	}
}

// Each extra target adds a report of up to --max-snapshot-bytes to every
// ingest and to the re-evaluation pass, so their number is what the
// server's memory bound is measured at (make test-heap): New refuses more
// than MaxExtraTargets distinct ones, and counts a target given twice once.
func TestNewRefusesMoreThanMaxExtraTargets(t *testing.T) {
	atCap := []string{"1.36", "1.37", "1.38", "1.39"}
	if len(atCap) != MaxExtraTargets {
		t.Fatalf("fixture has %d targets, want MaxExtraTargets (%d)", len(atCap), MaxExtraTargets)
	}
	if _, err := New(Config{Store: newFakeStore(), KB: testKB(), ExtraTargets: atCap}); err != nil {
		t.Fatalf("New with %d extra targets: %v", len(atCap), err)
	}
	if _, err := New(Config{Store: newFakeStore(), KB: testKB(), ExtraTargets: append(atCap, "v1.39", "1.37")}); err != nil {
		t.Fatalf("New with %d distinct extra targets, two given twice: %v", len(atCap), err)
	}
	_, err := New(Config{Store: newFakeStore(), KB: testKB(), ExtraTargets: append(atCap, "1.40")})
	if err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("New with %d extra targets: err = %v, want a refusal naming the limit of 4", len(atCap)+1, err)
	}
}
