package sarif

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

// Suppressed findings stay in the SARIF as results carrying an external
// suppression with the rule's reason, so code scanning shows them as
// dismissed rather than losing them; unanchored ones are notes.
func TestWriteSuppressed(t *testing.T) {
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

	if len(run.Results) != 2 {
		t.Fatalf("results = %+v, want the ComponentStatus result and the suppressed Ingress", run.Results)
	}
	if s := run.Results[0].Suppressions; len(s) != 0 {
		t.Errorf("unsuppressed result has suppressions %+v", s)
	}
	sup := run.Results[1]
	if sup.RuleID != "removed-api/networking.k8s.io/v1beta1/Ingress" || sup.Locations[0].PhysicalLocation.ArtifactLocation.URI != "legacy/old.yaml" {
		t.Errorf("suppressed result = %+v", sup)
	}
	if len(sup.Suppressions) != 1 || sup.Suppressions[0].Kind != "external" || sup.Suppressions[0].Justification != "legacy/ is not deployed" {
		t.Errorf("suppressions = %+v, want one external with the reason", sup.Suppressions)
	}
	if run.Properties.Suppressed != 2 || run.Properties.Findings != 1 {
		t.Errorf("properties = %+v, want findings 1, suppressed 2", run.Properties)
	}

	notes := run.Invocations[0].ToolExecutionNotifications
	if len(notes) != 1 || notes[0].Level != "note" || notes[0].Properties["findingKey"] != "eol-addon/ingress-nginx" ||
		!strings.Contains(notes[0].Message.Text, "Suppressed (migrating in Q1; until 2026-12-31)") {
		t.Errorf("notifications = %+v, want the suppressed add-on as a note", notes)
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
