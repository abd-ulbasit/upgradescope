package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// leadingFields decodes the first n members of a JSON object, in order.
func leadingFields(t *testing.T, raw []byte, n int) ([]string, []any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object (%v): %s", err, raw)
	}
	var names []string
	var values []any
	for range n {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("reading key: %v\n%s", err, raw)
		}
		name, _ := tok.(string)
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		names, values = append(names, name), append(values, v)
	}
	return names, values
}

// #60: the /api/v1 report-shaped responses (a cluster's report, stored or
// what-if, and the gate's JSON answer, with and without ?cluster=) lead
// with schemaVersion and toolVersion, as `scan --output json` does, so a
// consumer can tell which shape it reads and which server wrote it.
func TestReportResponsesLeadWithVersions(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.Version = "0.9.9-test" }).Handler())
	defer ts.Close()
	seedViaPush(t, ts)

	get := func(path string) []byte {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, resp.StatusCode, raw)
		}
		return raw
	}
	gate := func(query string) []byte {
		_, raw := postGate(t, ts, query, "", deploymentManifest, "application/x-yaml")
		return raw
	}
	for name, raw := range map[string][]byte{
		"stored report":  get("/api/v1/clusters/1/report?target=1.35"),
		"what-if report": get("/api/v1/clusters/1/report?target=1.40"),
		"gate":           gate("?target=1.35&fail-on=never"),
		"cluster gate":   gate("?target=1.35&fail-on=never&cluster=prod-eu-1"),
	} {
		names, values := leadingFields(t, raw, 2)
		if names[0] != "schemaVersion" || names[1] != "toolVersion" ||
			values[0] != float64(engine.ReportSchemaVersion) || values[1] != "0.9.9-test" {
			t.Errorf("%s leads with %v = %v, want schemaVersion = %d, toolVersion = 0.9.9-test", name, names, values, engine.ReportSchemaVersion)
		}
	}
}
