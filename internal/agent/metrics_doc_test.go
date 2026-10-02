package agent

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// TestMetricsReferenceListsEveryAgentMetric: docs/observability.md is the
// metrics reference. Every family the agent's registry serves after a
// full tick (Go runtime and process metrics aside) must be in it, so a new
// metric cannot ship undocumented.
func TestMetricsReferenceListsEveryAgentMetric(t *testing.T) {
	doc, err := os.ReadFile("../../docs/observability.md")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := newTestObserver(t, &now)
	o.record(tickReport{
		push: pushFailed, pushErr: os.ErrDeadlineExceeded, duration: time.Second,
		caps: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapAPIUsage: {Available: true, Partial: true},
		},
		reports: []engine.Report{{
			Target: inventory.Version{Major: 1, Minor: 38}, Verdict: engine.VerdictBlocked,
			Findings: []engine.Finding{{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker}},
		}},
	})
	families, err := o.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, mf := range families {
		name := mf.GetName()
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		n++
		if !strings.Contains(string(doc), "`"+name+"`") {
			t.Errorf("docs/observability.md does not document the agent metric %s", name)
		}
	}
	if n < 10 {
		t.Fatalf("gathered %d upgradescope metrics, want every one the agent registers", n)
	}
}
