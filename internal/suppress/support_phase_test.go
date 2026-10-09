package suppress

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// An EKS 1.34 cluster, judged on a given day with the embedded knowledge
// base (standard support ends 2026-12-02, extended support 2027-12-02).
func eksReportAt(t *testing.T, day string) engine.Report {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.Inventory{SchemaVersion: 1, ClusterID: "c", Source: inventory.SourceCluster, Provider: inventory.ProviderEKS, ServerVersion: "v1.34.2-eks-3abc123"}
	return engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 35}, at)
}

func supportFinding(t *testing.T, r engine.Report) engine.Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Category == engine.CatSupportLifecycle {
			return f
		}
	}
	t.Fatalf("no support-lifecycle finding in %+v", r.Findings)
	return engine.Finding{}
}

func blockers(r engine.Report) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == engine.SevBlocker {
			n++
		}
	}
	return n
}

// #266: the rule the docs recommend for staying in paid extended support
// accepts the extended-support blocker and stops there; it no longer hides
// the cluster falling out of support when extended support ends. With a
// fixed clock on both sides of standard_end and extended_end.
func TestSupportRulesAcceptOnePhase(t *testing.T) {
	ending := Rule{Key: "support-lifecycle/eks/1.34/ending", Reason: "upgrade scheduled"}
	extended := Rule{Key: "support-lifecycle/eks/1.34/extended", Reason: "stay and pay", Expires: "2027-12-01"}
	for _, c := range []struct {
		day       string
		phase     string
		rule      Rule
		suppress  bool // the rule takes the finding
		wantKey   string
		wantBlock int
	}{
		{"2026-12-01", "ending", ending, true, "support-lifecycle/eks/1.34/ending", 0},
		{"2026-12-01", "ending", extended, false, "support-lifecycle/eks/1.34/ending", 0},
		// The rule written for the ending warning does not take the blocker.
		{"2026-12-02", "extended", ending, false, "support-lifecycle/eks/1.34/extended", 1},
		{"2026-12-02", "extended", extended, true, "support-lifecycle/eks/1.34/extended", 0},
		{"2027-12-01", "extended", extended, true, "support-lifecycle/eks/1.34/extended", 0},
		// The day extended support ends: the out-of-support blocker is visible.
		{"2027-12-02", "ended", extended, false, "support-lifecycle/eks/1.34/ended", 1},
		{"2027-12-02", "ended", Rule{Key: "support-lifecycle/eks/1.34/ended", Reason: "decommission in Q1"}, true, "support-lifecycle/eks/1.34/ended", 0},
		{"2027-12-02", "ended", Rule{Category: "support-lifecycle", Reason: "all phases, on purpose"}, true, "support-lifecycle/eks/1.34/ended", 0},
	} {
		r := eksReportAt(t, c.day)
		f := supportFinding(t, r)
		if f.Key != c.wantKey {
			t.Errorf("%s: key = %s, want %s", c.day, f.Key, c.wantKey)
		}
		now, _ := time.Parse("2006-01-02", c.day)
		got, _ := Apply(r, []Rule{c.rule}, Options{Now: now, Source: "cfg"})
		suppressed := len(got.Suppressed) == 1
		if suppressed != c.suppress || blockers(got) != c.wantBlock {
			t.Errorf("%s (%s) with %s: suppressed = %v, visible blockers = %d; want %v and %d", c.day, c.phase, c.rule, suppressed, blockers(got), c.suppress, c.wantBlock)
		}
	}
}

// A baseline written the day before extended support ends does not hold
// the out-of-support blocker: the key moved, so it is new and fails the
// gate (#266). The same baseline holds the extended-support blocker it
// recorded.
func TestSupportBaselineDoesNotHoldALaterPhase(t *testing.T) {
	baselineOf := func(r engine.Report) Baseline {
		raw, err := json.Marshal(map[string]any{"schemaVersion": 1, "findings": r.Findings})
		if err != nil {
			t.Fatal(err)
		}
		b, err := ReadBaseline(bytes.NewReader(raw), 1)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := baselineOf(eksReportAt(t, "2027-12-01"))
	if f := supportFinding(t, b.Mark(eksReportAt(t, "2027-12-01"))); f.BaselineState != engine.BaselineUnchanged {
		t.Errorf("the same phase: baselineState = %q, want unchanged", f.BaselineState)
	}
	if f := supportFinding(t, b.Mark(eksReportAt(t, "2027-12-02"))); f.BaselineState != engine.BaselineNew || f.Severity != engine.SevBlocker {
		t.Errorf("the day extended support ends: baselineState = %q severity %s, want a new blocker", f.BaselineState, f.Severity)
	}
	// And a baseline from the ending warning does not hold the blocker
	// the same key used to carry.
	b = baselineOf(eksReportAt(t, "2026-12-01"))
	if f := supportFinding(t, b.Mark(eksReportAt(t, "2026-12-02"))); f.BaselineState != engine.BaselineNew {
		t.Errorf("standard support ends: baselineState = %q, want new", f.BaselineState)
	}
}

// A rule keyed as before (support-lifecycle/<provider>/<minor>) names no
// phase and matches no finding, which fails closed; Apply says so instead
// of leaving the rule silently dead.
func TestLegacySupportKeyRuleMatchesNothingAndSaysSo(t *testing.T) {
	r := eksReportAt(t, "2027-06-01")
	now := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	got, warnings := Apply(r, []Rule{{Key: "support-lifecycle/eks/1.34", Reason: "stay and pay"}}, Options{Now: now, Source: ".upgradescope.yaml"})
	if len(got.Suppressed) != 0 || blockers(got) != 1 {
		t.Errorf("suppressed %d, visible blockers %d; want the blocker visible", len(got.Suppressed), blockers(got))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "matches nothing") || !strings.Contains(warnings[0], "support-lifecycle/eks/1.34/extended") {
		t.Errorf("warnings = %q, want one naming the phase-qualified keys", warnings)
	}
	// Phase-qualified keys, and rules for other keys, draw no warning.
	for _, key := range []string{"support-lifecycle/eks/1.34/extended", "kb-stale", "eol-addon/ingress-nginx", "support-lifecycle/eks"} {
		if _, w := Apply(r, []Rule{{Key: key, Reason: "x"}}, Options{Now: now}); len(w) != 0 {
			t.Errorf("key %q: warnings = %q, want none", key, w)
		}
	}
}
