package suppress

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func mustBaseline(t *testing.T, doc string) Baseline {
	t.Helper()
	b, err := ReadBaseline(strings.NewReader(doc), 1)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A finding is unchanged only when the baseline had its key and every
// object it lists (identity: namespace, name, file — not line); anything
// else is new.
func TestBaselineMark(t *testing.T) {
	b := mustBaseline(t, `{
  "schemaVersion": 1,
  "findings": [
    {"category": "eol-addon", "severity": "blocker", "key": "eol-addon/ingress-nginx", "title": "old title"},
    {"category": "removed-api", "severity": "blocker", "key": "removed-api/networking.k8s.io/v1beta1/Ingress", "title": "t",
     "objects": [{"namespace": "shop", "name": "web", "file": "app.yaml", "line": 3}, {"namespace": "shop", "name": "admin", "file": "app.yaml", "line": 9}]}
  ]
}`)
	moved := shopWeb
	moved.Line = 40
	newObj := inventory.ObjectRef{Namespace: "shop", Name: "new", File: "app.yaml", Line: 12}
	omitted := removedIngress(shopWeb)
	omitted.ObjectsOmitted = 5

	cases := []struct {
		name string
		f    engine.Finding
		want engine.BaselineState
	}{
		{"same key, no objects", eolNginx(), engine.BaselineUnchanged},
		{"known objects, line moved", removedIngress(moved), engine.BaselineUnchanged},
		{"a new object", removedIngress(shopWeb, newObj), engine.BaselineNew},
		{"more objects than the baseline had", omitted, engine.BaselineNew},
		{"new key", staleKB(), engine.BaselineNew},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := engine.Report{Findings: []engine.Finding{tc.f}}
			got := b.Mark(r)
			if s := got.Findings[0].BaselineState; s != tc.want {
				t.Errorf("BaselineState = %q, want %q", s, tc.want)
			}
			if r.Findings[0].BaselineState != "" {
				t.Error("Mark modified its input report")
			}
		})
	}
}

func TestReadBaselineRejectsNonReports(t *testing.T) {
	cases := map[string]string{
		"not json":       "nope",
		"sarif":          `{"version": "2.1.0", "runs": []}`,
		"no findings":    `{"schemaVersion": 1}`,
		"newer schema":   `{"schemaVersion": 2, "findings": []}`,
		"findings wrong": `{"schemaVersion": 1, "findings": {}}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadBaseline(strings.NewReader(doc), 1); err == nil {
				t.Error("want error")
			}
		})
	}
}
