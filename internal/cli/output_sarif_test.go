package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

// Issue #122 (M08/VS-06): SARIF carried no notAssessed, so a code-scanning
// upload of a scan that could not see the add-ons read as a clean pass.
// The gaps and the verdict are in run.properties, and each gap is a tool
// execution notification: an error when it made the verdict unknown.
func TestWriteSARIFReportsGaps(t *testing.T) {
	gaps := []engine.CapabilityGap{
		{Capability: inventory.CapAddOns, Reason: "list pods: forbidden", Required: true},
		{Capability: inventory.CapAPIUsage, Reason: "list networking.k8s.io/v1 ingresses: forbidden", Partial: true,
			Skipped: []string{"networking.k8s.io/v1beta1 Ingress"}},
	}
	r := engine.Report{Target: inventory.Version{Major: 1, Minor: 37}, Score: 100, Verdict: engine.VerdictUnknown, NotAssessed: gaps}
	var buf bytes.Buffer
	if err := WriteSARIF(&buf, r); err != nil {
		t.Fatal(err)
	}
	sariftest.AssertGitHubAcceptable(t, buf.Bytes())

	var log struct {
		Runs []struct {
			Invocations []struct {
				ToolExecutionNotifications []struct {
					Level   string `json:"level"`
					Message struct {
						Text string `json:"text"`
					} `json:"message"`
					Properties map[string]string `json:"properties"`
				} `json:"toolExecutionNotifications"`
			} `json:"invocations"`
			Properties struct {
				Ready       bool                   `json:"ready"`
				Verdict     string                 `json:"verdict"`
				NotAssessed []engine.CapabilityGap `json:"notAssessed"`
			} `json:"properties"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(buf.Bytes(), &log); err != nil {
		t.Fatal(err)
	}
	run := log.Runs[0]
	if run.Properties.Ready || run.Properties.Verdict != "unknown" || !reflect.DeepEqual(run.Properties.NotAssessed, gaps) {
		t.Errorf("run.properties = %+v, want verdict unknown and the gaps", run.Properties)
	}
	notes := run.Invocations[0].ToolExecutionNotifications
	if len(notes) != 2 {
		t.Fatalf("notifications = %+v, want one per gap", notes)
	}
	want := []struct{ level, capability, text string }{
		{"error", "addons", "Not assessed: addons (required): list pods: forbidden"},
		{"note", "api-usage", "Not assessed: api-usage (partial): list networking.k8s.io/v1 ingresses: forbidden. Skipped: networking.k8s.io/v1beta1 Ingress."},
	}
	for i, w := range want {
		n := notes[i]
		if n.Level != w.level || n.Properties["capability"] != w.capability || !strings.HasPrefix(n.Message.Text, w.text) {
			t.Errorf("notification %d = %+v, want level %s, capability %s, text %q", i, n, w.level, w.capability, w.text)
		}
	}
}
