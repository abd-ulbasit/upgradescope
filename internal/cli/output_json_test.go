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

	// Canonical: indented, struct order (schemaVersion, toolVersion, then the
	// report fields), trailing newline.
	if !strings.Contains(out, "\n  \"toolVersion\": \""+version+"\",\n  \"clusterId\": \"files\",") {
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
	want := map[string]engine.TeamScore{"unattributed": {Score: 75, Ready: false, Verdict: engine.VerdictBlocked, Blockers: 1}}
	if len(withTeams.Teams) != 1 || withTeams.Teams["unattributed"] != want["unattributed"] {
		t.Errorf("teams = %+v, want %+v", withTeams.Teams, want)
	}
}

// The report states its shape version and the producing binary, ahead of
// the existing fields, which keep their names and order.
func TestWriteJSONSchemaAndToolVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, engine.Report{ClusterID: "c", Target: inventory.Version{Major: 1, Minor: 36}}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	want := "{\n  \"schemaVersion\": 1,\n  \"toolVersion\": \"" + version + "\",\n  \"clusterId\": \"c\","
	if !strings.HasPrefix(buf.String(), want) {
		t.Errorf("JSON head:\n%s\nwant prefix:\n%s", buf.String(), want)
	}
}

// A partial gap carries its partial flag and what it skipped (issue #122).
func TestWriteJSONPartialGap(t *testing.T) {
	var buf bytes.Buffer
	r := engine.Report{Target: inventory.Version{Major: 1, Minor: 25}, Verdict: engine.VerdictUnknown, NotAssessed: []engine.CapabilityGap{{
		Capability: inventory.CapAPIUsage, Reason: "list policy/v1beta1 podsecuritypolicies: forbidden",
		Partial: true, Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}, Required: true,
	}}}
	if err := WriteJSON(&buf, r); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	const want = `"notAssessed": [
    {
      "capability": "api-usage",
      "reason": "list policy/v1beta1 podsecuritypolicies: forbidden",
      "partial": true,
      "skipped": [
        "policy/v1beta1 PodSecurityPolicy"
      ],
      "required": true
    }
  ]`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("JSON lacks the partial gap:\n%s", buf.String())
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
