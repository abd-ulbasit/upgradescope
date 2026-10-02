package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestMetricsReferenceListsEveryServerMetric: docs/observability.md is the
// metrics reference. Every family the server's registry serves once it
// holds a cluster and has answered requests (Go runtime and process
// metrics aside) must be in it, so a new metric cannot ship undocumented.
func TestMetricsReferenceListsEveryServerMetric(t *testing.T) {
	doc, err := os.ReadFile("../../docs/observability.md")
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, newFakeStore())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	if resp := getJSON(t, ts, "/api/v1/clusters", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/clusters = %d", resp.StatusCode)
	}
	families, err := s.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, mf := range families {
		name := mf.GetName()
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		n++
		if !strings.Contains(string(doc), "`"+name+"`") {
			t.Errorf("docs/observability.md does not document the server metric %s", name)
		}
	}
	if n < 8 {
		t.Fatalf("gathered %d upgradescope metrics, want every one the server registers", n)
	}
}
