package crd

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestTargetStatusFromReportCounts(t *testing.T) {
	r := engine.Report{
		Target: inventory.Version{Major: 1, Minor: 36},
		Score:  72,
		Ready:  false,
		Findings: []engine.Finding{
			{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: "b1", Remediation: "fix b1"},
			{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: "b2"},
			{Category: engine.CatEOLApproaching, Severity: engine.SevWarning, Title: "w1"},
			{Category: engine.CatDeprecatedAPI, Severity: engine.SevInfo, Title: "i1"},
		},
	}
	ts := TargetStatusFromReport(r)
	if ts.Target != "1.36" {
		t.Errorf("Target = %q, want 1.36", ts.Target)
	}
	if ts.Score != 72 || ts.Ready {
		t.Errorf("Score/Ready = %d/%v, want 72/false", ts.Score, ts.Ready)
	}
	if ts.Blockers != 2 || ts.Warnings != 1 || ts.Infos != 1 {
		t.Errorf("counts = %d/%d/%d, want 2/1/1", ts.Blockers, ts.Warnings, ts.Infos)
	}
	wantByCat := map[string]int{"removed-api": 2, "eol-approaching": 1, "deprecated-api": 1}
	for k, v := range wantByCat {
		if ts.ByCategory[k] != v {
			t.Errorf("ByCategory[%s] = %d, want %d", k, ts.ByCategory[k], v)
		}
	}
	if len(ts.TopFindings) != 4 {
		t.Fatalf("TopFindings len = %d, want 4", len(ts.TopFindings))
	}
	if ts.TopFindings[0].Title != "b1" || ts.TopFindings[0].Remediation != "fix b1" || ts.TopFindings[0].Severity != "blocker" {
		t.Errorf("TopFindings[0] = %+v, want b1 blocker with remediation", ts.TopFindings[0])
	}
}

func TestTargetStatusFromReportTop20Cap(t *testing.T) {
	r := engine.Report{Target: inventory.Version{Major: 1, Minor: 36}}
	for i := 0; i < 25; i++ {
		r.Findings = append(r.Findings, engine.Finding{
			Category: engine.CatDeprecatedAPI,
			Severity: engine.SevWarning,
			Title:    fmt.Sprintf("finding-%02d", i),
		})
	}
	ts := TargetStatusFromReport(r)
	if len(ts.TopFindings) != 20 {
		t.Fatalf("TopFindings len = %d, want 20 (cap)", len(ts.TopFindings))
	}
	// Report.Findings is severity-sorted per engine contract; the cap keeps the head.
	if ts.TopFindings[0].Title != "finding-00" || ts.TopFindings[19].Title != "finding-19" {
		t.Errorf("cap kept wrong findings: first=%q last=%q", ts.TopFindings[0].Title, ts.TopFindings[19].Title)
	}
	if ts.Warnings != 25 {
		t.Errorf("Warnings = %d, want 25 (counts are not capped)", ts.Warnings)
	}
}

func TestStatusFromReports(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	reports := []engine.Report{
		{
			Target:    inventory.Version{Major: 1, Minor: 36},
			KBVersion: "k8s-1.36+registry-2026-06-10",
			Score:     90,
			Ready:     true,
			NotAssessed: []engine.CapabilityGap{
				{Capability: inventory.CapHelm, Reason: "secrets list forbidden"},
				{Capability: inventory.CapDeprecatedCalls, Reason: "metrics scrape forbidden"},
			},
		},
		{Target: inventory.Version{Major: 1, Minor: 37}, KBVersion: "k8s-1.36+registry-2026-06-10", Score: 70},
	}
	st := StatusFromReports(reports, "v1.35.2", "v0.2.0", now)
	if st.ObservedServerVersion != "v1.35.2" {
		t.Errorf("ObservedServerVersion = %q", st.ObservedServerVersion)
	}
	if st.KBVersion != "k8s-1.36+registry-2026-06-10" {
		t.Errorf("KBVersion = %q", st.KBVersion)
	}
	if st.AgentVersion != "v0.2.0" {
		t.Errorf("AgentVersion = %q", st.AgentVersion)
	}
	if !st.LastEvaluated.Time.Equal(now) {
		t.Errorf("LastEvaluated = %v, want %v", st.LastEvaluated, now)
	}
	if len(st.Targets) != 2 || st.Targets[0].Target != "1.36" || st.Targets[1].Target != "1.37" {
		t.Fatalf("Targets = %+v, want [1.36 1.37]", st.Targets)
	}
	want := []string{"helm: secrets list forbidden", "deprecated-calls: metrics scrape forbidden"}
	if len(st.NotAssessed) != len(want) || st.NotAssessed[0] != want[0] || st.NotAssessed[1] != want[1] {
		t.Errorf("NotAssessed = %v, want %v", st.NotAssessed, want)
	}
}

func TestTargetStatusFromReportVerdict(t *testing.T) {
	for _, v := range []engine.Verdict{engine.VerdictReady, engine.VerdictBlocked, engine.VerdictUnknown} {
		ts := TargetStatusFromReport(engine.Report{
			Target: inventory.Version{Major: 1, Minor: 36}, Verdict: v, Ready: v == engine.VerdictReady,
		})
		if ts.Verdict != string(v) || ts.Ready != (v == engine.VerdictReady) {
			t.Errorf("verdict %q: TargetStatus verdict/ready = %q/%v", v, ts.Verdict, ts.Ready)
		}
	}
}

// Issue #122: a partial capability reaches status.notAssessed marked
// partial, with what it skipped, and the Ready condition message names
// the first target's gaps, required ones included, whatever the verdict.
func TestStatusFromReportsPartialGaps(t *testing.T) {
	partial := engine.CapabilityGap{Capability: inventory.CapAPIUsage, Reason: "list policy/v1beta1 podsecuritypolicies: forbidden",
		Partial: true, Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}}
	required := partial
	required.Required = true
	calls := engine.CapabilityGap{Capability: inventory.CapDeprecatedCalls, Reason: "GET /metrics forbidden"}
	st := StatusFromReports([]engine.Report{
		{Target: inventory.Version{Major: 1, Minor: 25}, Score: 100, Verdict: engine.VerdictUnknown, NotAssessed: []engine.CapabilityGap{required, calls}},
		{Target: inventory.Version{Major: 1, Minor: 24}, Score: 100, Verdict: engine.VerdictReady, Ready: true, NotAssessed: []engine.CapabilityGap{partial, calls}},
	}, "v1.24.17", "v0.2.0", time.Now())
	want := []string{ // one entry per gap: required-ness is per target, the condition says it
		"api-usage (partial): list policy/v1beta1 podsecuritypolicies: forbidden; skipped: policy/v1beta1 PodSecurityPolicy",
		"deprecated-calls: GET /metrics forbidden",
	}
	if !reflect.DeepEqual(st.NotAssessed, want) {
		t.Errorf("NotAssessed = %q\nwant          %q", st.NotAssessed, want)
	}
	c := ReadyCondition(st)
	const msg = "1.25: no blockers found, but a required check was not assessed: api-usage (partial, required), deprecated-calls; see status.notAssessed (score 100)"
	if c.Status != metav1.ConditionUnknown || c.Message != msg {
		t.Errorf("condition = %s %q\nwant Unknown %q", c.Status, c.Message, msg)
	}

	st = StatusFromReports([]engine.Report{
		{Target: inventory.Version{Major: 1, Minor: 24}, Score: 100, Verdict: engine.VerdictReady, Ready: true, NotAssessed: []engine.CapabilityGap{partial}},
	}, "v1.23.17", "v0.2.0", time.Now())
	if c := ReadyCondition(st); c.Status != metav1.ConditionTrue || c.Message != "1.24: ready (score 100); not fully assessed: api-usage (partial); see status.notAssessed" {
		t.Errorf("condition = %s %q, want True, naming the partial gap", c.Status, c.Message)
	}
}

// Gaps can be target-specific (kb-coverage only for targets beyond the KB),
// so status.notAssessed is the union over all targets, not the first one's.
func TestStatusFromReportsUnionsNotAssessed(t *testing.T) {
	helm := engine.CapabilityGap{Capability: inventory.CapHelm, Reason: "secrets list forbidden"}
	kbGap := engine.CapabilityGap{Capability: engine.GapKBCoverage, Reason: "knowledge base covers Kubernetes up to 1.37; target 1.38 cannot be assessed", Required: true}
	st := StatusFromReports([]engine.Report{
		{Target: inventory.Version{Major: 1, Minor: 37}, NotAssessed: []engine.CapabilityGap{helm}},
		{Target: inventory.Version{Major: 1, Minor: 38}, NotAssessed: []engine.CapabilityGap{helm, kbGap}},
	}, "v1.36.0", "v0.2.0", time.Now())
	want := []string{
		"helm: secrets list forbidden",
		"kb-coverage: knowledge base covers Kubernetes up to 1.37; target 1.38 cannot be assessed",
	}
	if len(st.NotAssessed) != len(want) || st.NotAssessed[0] != want[0] || st.NotAssessed[1] != want[1] {
		t.Errorf("NotAssessed = %q, want %q", st.NotAssessed, want)
	}
}

func TestStatusFromReportsEmpty(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	st := StatusFromReports(nil, "v1.35.2", "v0.2.0", now)
	if len(st.Targets) != 0 || len(st.NotAssessed) != 0 || st.KBVersion != "" {
		t.Errorf("empty reports produced non-empty derived fields: %+v", st)
	}
	if st.ObservedServerVersion != "v1.35.2" || st.AgentVersion != "v0.2.0" {
		t.Errorf("base fields missing: %+v", st)
	}
}
