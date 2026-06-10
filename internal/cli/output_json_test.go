package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestWriteJSON(t *testing.T) {
	r := engine.Report{
		ClusterID: "files",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     75,
		Ready:     false,
		Findings: []engine.Finding{
			{Category: engine.CatEOLAddon, Severity: engine.SevBlocker, Title: "ingress-nginx is past end of life"},
		},
	}

	var buf bytes.Buffer
	if err := WriteJSON(&buf, r); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	out := buf.String()

	// Canonical: indented, clusterId first (struct order), trailing newline.
	if !strings.HasPrefix(out, "{\n  \"clusterId\": \"files\",") {
		t.Errorf("not canonical indented JSON, got:\n%s", out)
	}
	if !strings.HasSuffix(out, "}\n") {
		t.Errorf("missing trailing newline, got:\n%q", out[len(out)-5:])
	}

	var back engine.Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if back.Score != 75 || back.Ready || back.KBVersion != "test-kb" || len(back.Findings) != 1 {
		t.Errorf("round-trip mismatch: %+v", back)
	}

	// Presentation-time teams field: the blocker has no team → bucket
	// "unattributed".
	var withTeams struct {
		Teams map[string]engine.TeamScore `json:"teams"`
	}
	if err := json.Unmarshal(buf.Bytes(), &withTeams); err != nil {
		t.Fatalf("unmarshal teams: %v", err)
	}
	want := map[string]engine.TeamScore{"unattributed": {Score: 75, Ready: false, Blockers: 1}}
	if len(withTeams.Teams) != 1 || withTeams.Teams["unattributed"] != want["unattributed"] {
		t.Errorf("teams = %+v, want %+v", withTeams.Teams, want)
	}
}

// A report with no findings emits no teams key at all (omitempty).
func TestWriteJSONNoFindingsOmitsTeams(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, engine.Report{ClusterID: "c", Target: inventory.Version{Major: 1, Minor: 36}}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if strings.Contains(buf.String(), "\"teams\"") {
		t.Errorf("teams key present for empty report:\n%s", buf.String())
	}
}
