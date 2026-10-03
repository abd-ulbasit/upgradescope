package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// resolve compiles a schema document the way the MCP SDK does for a tool's
// outputSchema, so these tests judge documents by the same validator that
// judges a tool's output.
func resolve(t *testing.T, schema any) *jsonschema.Resolved {
	t.Helper()
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("schema does not resolve: %v", err)
	}
	return r
}

func validate(t *testing.T, r *jsonschema.Resolved, doc []byte) error {
	t.Helper()
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		t.Fatal(err)
	}
	return r.Validate(&v)
}

// withEnvelope adds what the CLI writes around an engine report
// (schemaVersion, toolVersion), which the engine's golden files omit.
func withEnvelope(t *testing.T, golden []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(golden, &m); err != nil {
		t.Fatal(err)
	}
	m["schemaVersion"] = 1
	m["toolVersion"] = "test"
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestReportSchemaValidatesGoldenReports is the issue's contract test: every
// golden report the engine produces, once the CLI's envelope is on it, is a
// document api/report.schema.json accepts, and so is every one of its
// findings under the findings tool's schema.
func TestReportSchemaValidatesGoldenReports(t *testing.T) {
	report := resolve(t, ReportOutputSchema())
	findings := resolve(t, FindingsOutputSchema())
	files, err := filepath.Glob("../engine/testdata/*/expected.json")
	if err != nil || len(files) < 10 {
		t.Fatalf("golden reports: %d files, %v", len(files), err)
	}
	for _, f := range files {
		t.Run(filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			doc := withEnvelope(t, raw)
			if err := validate(t, report, doc); err != nil {
				t.Fatalf("report does not validate against api/report.schema.json: %v", err)
			}
			var r struct {
				Target   string            `json:"target"`
				Findings []json.RawMessage `json:"findings"`
			}
			if err := json.Unmarshal(doc, &r); err != nil {
				t.Fatal(err)
			}
			out, _ := json.Marshal(map[string]any{"target": r.Target, "findings": r.Findings, "total": len(r.Findings), "truncated": false})
			if err := validate(t, findings, out); err != nil {
				t.Fatalf("findings result does not validate: %v", err)
			}
		})
	}
}

// TestOutputSchemasRejectWhatTheContractForbids: the schemas are the real
// contract, not "anything object-shaped".
func TestOutputSchemasRejectWhatTheContractForbids(t *testing.T) {
	report := resolve(t, ReportOutputSchema())
	if err := validate(t, report, []byte(`{"schemaVersion":1}`)); err == nil {
		t.Error("a report without its required fields validates")
	}
	findings := resolve(t, FindingsOutputSchema())
	bad := `{"target":"1.37","total":1,"truncated":false,"findings":[{"category":"eol-addon","severity":"catastrophic","title":"t","detail":"d"}]}`
	if err := validate(t, findings, []byte(bad)); err == nil {
		t.Error("a finding with an unknown severity validates")
	}
	scan := resolve(t, ScanOutputSchema())
	if err := validate(t, scan, []byte(`{"reports":[{"schemaVersion":1}]}`)); err == nil {
		t.Error("a scan result holding an invalid report validates")
	}
}
