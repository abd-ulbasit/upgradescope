package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestWriteSARIF(t *testing.T) {
	r := engine.Report{
		ClusterID: "prod-eu-1",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Findings: []engine.Finding{
			{
				Category: engine.CatEOLAddon, Severity: engine.SevBlocker,
				Title: "ingress-nginx is past end of life", Detail: "EOL 2026-03-24",
				Namespaces: []string{"ingress-nginx"},
			},
			{
				Category: engine.CatVersionSkew, Severity: engine.SevWarning,
				Title: "kubelet 3 minors behind apiserver", Detail: "node worker-1 is v1.33.0",
			},
			{
				Category: engine.CatDeprecatedAPI, Severity: engine.SevInfo,
				Title: "batch/v1beta1 CronJob is deprecated",
			},
		},
	}

	var buf bytes.Buffer
	if err := WriteSARIF(&buf, r); err != nil {
		t.Fatalf("WriteSARIF: %v", err)
	}

	var log sarifLog
	if err := json.Unmarshal(buf.Bytes(), &log); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if log.Version != "2.1.0" {
		t.Errorf("version = %q, want 2.1.0", log.Version)
	}
	if len(log.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(log.Runs))
	}
	run := log.Runs[0]
	if run.Tool.Driver.Name != "upgradescope" {
		t.Errorf("driver name = %q, want upgradescope", run.Tool.Driver.Name)
	}
	if len(run.Tool.Driver.Rules) != 3 { // one rule per distinct category
		t.Errorf("rules = %d, want 3", len(run.Tool.Driver.Rules))
	}
	if run.Tool.Driver.Rules[0].ID != "eol-addon" {
		t.Errorf("rules[0].id = %q, want eol-addon", run.Tool.Driver.Rules[0].ID)
	}
	if len(run.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(run.Results))
	}
	wantLevels := []string{"error", "warning", "note"} // blocker, warning, info
	for i, res := range run.Results {
		if res.Level != wantLevels[i] {
			t.Errorf("results[%d].level = %q, want %q", i, res.Level, wantLevels[i])
		}
	}
	if got := run.Results[0].Message.Text; got != "ingress-nginx is past end of life — EOL 2026-03-24" {
		t.Errorf("message.text = %q", got)
	}
	// Detail empty → title only, no dangling separator.
	if got := run.Results[2].Message.Text; got != "batch/v1beta1 CronJob is deprecated" {
		t.Errorf("message.text = %q", got)
	}
	// Cluster mode: logical locations only, namespace kind.
	locs := run.Results[0].Locations
	if len(locs) != 1 || len(locs[0].LogicalLocations) != 1 ||
		locs[0].LogicalLocations[0].Name != "ingress-nginx" ||
		locs[0].LogicalLocations[0].Kind != "namespace" {
		t.Errorf("results[0].locations = %+v", locs)
	}
	if len(run.Results[1].Locations) != 0 { // no namespaces → no locations
		t.Errorf("results[1] should have no locations, got %+v", run.Results[1].Locations)
	}
}
