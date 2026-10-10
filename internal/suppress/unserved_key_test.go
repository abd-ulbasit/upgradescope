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

var unservedNow = time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)

// A manifest of networking.k8s.io/v1beta1 ServiceCIDR (served from 1.31,
// removed in 1.37), scanned at target 1.<minor>.
func serviceCIDRReport(t *testing.T, minor int) engine.Report {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.Inventory{SchemaVersion: 1, ClusterID: "files", Source: inventory.SourceFiles, APIUsage: []inventory.APIUsage{{
		Group: "networking.k8s.io", Version: "v1beta1", Kind: "ServiceCIDR", Count: 1, Namespaces: map[string]int{"": 1},
		Objects: []inventory.ObjectRef{{Name: "extra", File: "g/sc.yaml", Line: 1}},
	}}}
	return engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: minor}, unservedNow)
}

func apiFinding(t *testing.T, r engine.Report) engine.Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Category == engine.CatRemovedAPI {
			return f
		}
	}
	t.Fatalf("no removed-api finding in %+v", r.Findings)
	return engine.Finding{}
}

func baselineFromReport(t *testing.T, r engine.Report) Baseline {
	t.Helper()
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

// #300: the rule written for the "not served until 1.31" blocker at target
// 1.30 no longer takes the removal blocker at 1.37, and the baseline taken
// at 1.30 marks it new (the gate fails).
func TestRuleAndBaselineForUnservedDoNotHoldTheRemoval(t *testing.T) {
	early, late := serviceCIDRReport(t, 30), serviceCIDRReport(t, 37)
	unserved := apiFinding(t, early)
	if !strings.HasSuffix(unserved.Key, "/unserved") || !strings.Contains(unserved.Title, "is not served until 1.31") {
		t.Fatalf("1.30 finding = %q %q, want the unserved blocker", unserved.Key, unserved.Title)
	}
	rule := Rule{Key: unserved.Key, Reason: "applied once the cluster is on 1.31"}

	got, warnings := Apply(early, []Rule{rule}, Options{Now: unservedNow, Source: ".upgradescope.yaml"})
	if len(got.Suppressed) != 1 || blockers(got) != 0 || len(warnings) != 0 {
		t.Errorf("target 1.30: suppressed %d, visible blockers %d, warnings %q; want the rule to take its blocker", len(got.Suppressed), blockers(got), warnings)
	}
	got, _ = Apply(late, []Rule{rule}, Options{Now: unservedNow})
	if len(got.Suppressed) != 0 || blockers(got) != 1 || got.Ready {
		t.Errorf("target 1.37: suppressed %d, visible blockers %d, ready %v; want the removal blocker visible", len(got.Suppressed), blockers(got), got.Ready)
	}

	b := baselineFromReport(t, early)
	if f := apiFinding(t, b.Mark(early)); f.BaselineState != engine.BaselineUnchanged {
		t.Errorf("1.30 against its own baseline: %q, want unchanged", f.BaselineState)
	}
	if f := apiFinding(t, b.Mark(late)); f.BaselineState != engine.BaselineNew || f.Severity != engine.SevBlocker {
		t.Errorf("1.37 against the 1.30 baseline: %q %s, want a new blocker", f.BaselineState, f.Severity)
	}
}

// The next-minor removal warning and the removal blocker share one key, the
// documented contract: an ignore rule accepts a key whatever its severity.
// A baseline does not (it is severity-aware).
func TestRemovalWarningAndBlockerShareTheirKey(t *testing.T) {
	warn, block := serviceCIDRReport(t, 36), serviceCIDRReport(t, 37)
	wf, bf := apiFinding(t, warn), apiFinding(t, block)
	if wf.Severity != engine.SevWarning || bf.Severity != engine.SevBlocker || wf.Key != bf.Key || strings.HasSuffix(wf.Key, "/unserved") {
		t.Fatalf("warning %s %q, blocker %s %q; want one bare key", wf.Severity, wf.Key, bf.Severity, bf.Key)
	}
	got, _ := Apply(block, []Rule{{Key: wf.Key, Reason: "rule written for the warning"}}, Options{Now: unservedNow})
	if len(got.Suppressed) != 1 {
		t.Errorf("an ignore rule on the shared key took %d findings, want 1 (any severity)", len(got.Suppressed))
	}
	if f := apiFinding(t, baselineFromReport(t, warn).Mark(block)); f.BaselineState != engine.BaselineNew {
		t.Errorf("a baseline holding the warning marks the blocker %q, want new", f.BaselineState)
	}
}

// A rule with the bare key of an API whose current finding is the
// not-served-yet one matches nothing; Apply says what key to write, once.
func TestBareKeyRuleForUnservedAPIWarns(t *testing.T) {
	early := serviceCIDRReport(t, 30)
	bare := "removed-api/networking.k8s.io/v1beta1/ServiceCIDR"
	rule := Rule{Key: bare, Reason: "applied once the cluster is on 1.31"}
	got, warnings := Apply(early, []Rule{rule}, Options{Now: unservedNow, Source: ".upgradescope.yaml"})
	if len(got.Suppressed) != 0 || blockers(got) != 1 {
		t.Errorf("suppressed %d, visible blockers %d; want the bare-key rule to match nothing", len(got.Suppressed), blockers(got))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], bare+"/unserved") || !strings.Contains(warnings[0], "matches nothing") {
		t.Errorf("warnings = %q, want one naming %s/unserved", warnings, bare)
	}
	// At 1.37 the bare key is right: no warning, and it takes the removal.
	got, warnings = Apply(serviceCIDRReport(t, 37), []Rule{rule}, Options{Now: unservedNow})
	if len(warnings) != 0 || len(got.Suppressed) != 1 {
		t.Errorf("1.37: warnings %q, suppressed %d; want none and 1", warnings, len(got.Suppressed))
	}
	// A rule by category, or by the unserved key, draws no warning.
	for _, r := range []Rule{{Category: "removed-api", Reason: "x"}, {Key: bare + "/unserved", Reason: "x"}} {
		if _, w := Apply(early, []Rule{r}, Options{Now: unservedNow}); len(w) != 0 {
			t.Errorf("rule %s: warnings = %q, want none", r, w)
		}
	}
}

// A baseline written before the phase was part of the key holds the
// unserved blocker under the bare key; read as is, it would hold the
// removal blocker of the same API at a later target. It does not: its
// entry for the unserved finding is dropped, so that finding resurfaces
// once and the removal is new.
func TestOldBaselineDoesNotHoldARemovalByAnUnservedEntry(t *testing.T) {
	early := serviceCIDRReport(t, 30)
	f := apiFinding(t, early)
	f.Key = "removed-api/networking.k8s.io/v1beta1/ServiceCIDR" // as an older release wrote it
	old := early
	old.Findings = []engine.Finding{f}
	b := baselineFromReport(t, old)
	if got := apiFinding(t, b.Mark(serviceCIDRReport(t, 37))); got.BaselineState != engine.BaselineNew {
		t.Errorf("1.37 removal against an old 1.30 baseline: %q, want new", got.BaselineState)
	}
	if got := apiFinding(t, b.Mark(early)); got.BaselineState != engine.BaselineNew {
		t.Errorf("1.30 unserved against the old baseline: %q, want new (resurfaces once)", got.BaselineState)
	}
	// An old baseline's removal blocker is untouched.
	old = serviceCIDRReport(t, 37)
	if got := apiFinding(t, baselineFromReport(t, old).Mark(serviceCIDRReport(t, 37))); got.BaselineState != engine.BaselineUnchanged {
		t.Errorf("1.37 removal against its own baseline: %q, want unchanged", got.BaselineState)
	}
}
