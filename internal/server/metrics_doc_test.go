package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
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
	// Retention on and one prune done, so the retention series exist.
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Retention = 90 * 24 * time.Hour })
	s.pruneOnce(context.Background())
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
	if n < 11 {
		t.Fatalf("gathered %d upgradescope metrics, want every one the server registers", n)
	}
}
