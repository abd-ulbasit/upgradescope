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

// A finding is unchanged only when the baseline had its key at the same
// or a higher severity and every object it lists (identity: namespace,
// name, file — not line); anything else is new.
func TestBaselineMark(t *testing.T) {
	b := mustBaseline(t, `{
  "schemaVersion": 1,
  "findings": [
    {"category": "eol-addon", "severity": "blocker", "key": "eol-addon/ingress-nginx", "title": "old title"},
    {"category": "removed-api", "severity": "blocker", "key": "removed-api/networking.k8s.io/v1beta1/Ingress", "title": "t",
     "objects": [{"namespace": "shop", "name": "web", "file": "app.yaml", "line": 3}, {"namespace": "shop", "name": "admin", "file": "app.yaml", "line": 9}]},
    {"category": "kb-stale", "severity": "info", "key": "kb-stale", "title": "t"},
    {"category": "version-skew", "severity": "blocker", "key": "version-skew/kube-scheduler", "title": "newer"},
    {"category": "version-skew", "severity": "warning", "key": "version-skew/kube-scheduler", "title": "behind"},
    {"category": "deprecated-api", "key": "deprecated-api/no-severity", "title": "t"}
  ]
}`)
	moved := shopWeb
	moved.Line = 40
	newObj := inventory.ObjectRef{Namespace: "shop", Name: "new", File: "app.yaml", Line: 12}
	omitted := removedIngress(shopWeb)
	omitted.ObjectsOmitted = 5
	warningIngress := removedIngress(shopWeb)
	warningIngress.Severity = engine.SevWarning

	cases := []struct {
		name string
		f    engine.Finding
		want engine.BaselineState
	}{
		{"same key, no objects", eolNginx(), engine.BaselineUnchanged},
		{"known objects, line moved", removedIngress(moved), engine.BaselineUnchanged},
		{"a new object", removedIngress(shopWeb, newObj), engine.BaselineNew},
		{"more objects than the baseline had", omitted, engine.BaselineNew},
		{"new key", engine.Finding{Category: engine.CatEOLAddon, Severity: engine.SevBlocker, Key: "eol-addon/istio"}, engine.BaselineNew},
		{"severity rose", staleKB(), engine.BaselineNew},
		{"severity fell", warningIngress, engine.BaselineUnchanged},
		// A pre-0.2 baseline used one key for two skew findings.
		{"key baselined twice, highest severity counts", engine.Finding{Category: engine.CatVersionSkew, Severity: engine.SevBlocker, Key: "version-skew/kube-scheduler"}, engine.BaselineUnchanged},
		{"baselined without a severity", engine.Finding{Category: engine.CatDeprecatedAPI, Severity: engine.SevInfo, Key: "deprecated-api/no-severity"}, engine.BaselineNew},
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
