package sarif

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

// GitHub code scanning ignores a result's "suppressions" and opens an
// alert for every located result at its level (#375), so suppressed
// findings are not results at all. Located ones are counted in one
// run-level note; unanchored ones stay notes (never alerts); the
// unsuppressed result is untouched.
func TestWriteSuppressedOmitted(t *testing.T) {
	r := testReport()
	ing := r.Findings[1]
	ing.Objects = []inventory.ObjectRef{{Namespace: "shop", Name: "old", File: "legacy/old.yaml", Line: 2}}
	ing.ObjectsOmitted = 0
	r.Findings = r.Findings[4:] // keep the ComponentStatus result
	r.Suppressed = []engine.SuppressedFinding{
		{Finding: ing, Reason: "legacy/ is not deployed", Source: ".upgradescope.yaml"},
		{Finding: testReport().Findings[0], Reason: "migrating in Q1", Source: ".upgradescope.yaml", Expires: "2026-12-31"},
	}
	log, raw := writeLog(t, r)
	sariftest.AssertGitHubAcceptable(t, raw)
	run := log.Runs[0]

	if len(run.Results) != 1 || run.Results[0].RuleID == "removed-api/networking.k8s.io/v1beta1/Ingress" {
		t.Fatalf("results = %+v, want only the unsuppressed ComponentStatus result", run.Results)
	}
	for _, rl := range run.Tool.Driver.Rules {
		if rl.ID == "removed-api/networking.k8s.io/v1beta1/Ingress" {
			t.Errorf("rule %s listed although it has no result", rl.ID)
		}
	}
	if strings.Contains(string(raw), `"suppressions"`) || strings.Contains(string(raw), "legacy/old.yaml") {
		t.Errorf("SARIF still carries the suppressed located finding:\n%s", raw)
	}
	if run.Properties.Suppressed != 2 || run.Properties.Findings != 1 {
		t.Errorf("properties = %+v, want findings 1, suppressed 2", run.Properties)
	}

	var texts []string
	for _, n := range run.Invocations[0].ToolExecutionNotifications {
		if n.Level != "note" {
			t.Errorf("notification %+v is not a note", n)
		}
		texts = append(texts, n.Message.Text)
	}
	all := strings.Join(texts, "\n")
	if !strings.Contains(all, "1 finding accepted by ignore rules or annotations is not uploaded; see the JSON report") {
		t.Errorf("notifications lack the run-level count:\n%s", all)
	}
	if !strings.Contains(all, "Suppressed (migrating in Q1; until 2026-12-31)") {
		t.Errorf("notifications lack the unanchored suppressed add-on:\n%s", all)
	}
}

// With nothing suppressed there is no run-level note.
func TestWriteNoSuppressedNoNote(t *testing.T) {
	_, raw := writeLog(t, testReport())
	if strings.Contains(string(raw), "accepted by ignore rules") {
		t.Errorf("note emitted without suppressed findings:\n%s", raw)
	}
}

// A report compared with a baseline marks each result unchanged or new.
func TestWriteBaselineState(t *testing.T) {
	r := testReport()
	for i := range r.Findings {
		r.Findings[i].BaselineState = engine.BaselineNew
	}
	r.Findings[1].BaselineState = engine.BaselineUnchanged
	log, raw := writeLog(t, r)
	sariftest.AssertGitHubAcceptable(t, raw)
	var states []string
	for _, res := range log.Runs[0].Results {
		states = append(states, res.BaselineState)
	}
	if got := strings.Join(states, ","); got != "unchanged,unchanged,new" {
		t.Errorf("baselineState = %s, want unchanged,unchanged,new", got)
	}

	// Without a baseline the property is absent.
	_, raw = writeLog(t, testReport())
	if strings.Contains(string(raw), "baselineState") {
		t.Error("baselineState emitted without a baseline")
	}
}
