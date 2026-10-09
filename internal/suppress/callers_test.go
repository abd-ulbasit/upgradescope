package suppress

import (
	"reflect"
	"slices"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

const (
	flowSchemaKey = "removed-api/flowcontrol.apiserver.k8s.io/v1beta3/FlowSchema"
	callerKey     = "deprecated-api-in-use/flowcontrol.apiserver.k8s.io/v1beta3/flowschemas"
)

// callerReport is #236's repro: one stored FlowSchema written through
// flowcontrol v1beta3 (removed in 1.32), and a caller row for the same
// API folded into its finding, evaluated at 1.32.
func callerReport(t *testing.T, obj inventory.ObjectRef) engine.Report {
	t.Helper()
	v := func(m int) *inventory.Version { return &inventory.Version{Major: 1, Minor: m} }
	k := kb.KB{
		Version: "test-kb",
		APILifecycle: []kb.APILifecycleEntry{
			{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Introduced: *v(26), Deprecated: v(29), Removed: v(32),
				Replacement: &kb.GVK{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "FlowSchema"}},
			{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "FlowSchema", Introduced: *v(29)},
		},
		Skew:        kb.DefaultSkewPolicy(),
		MaxKnownK8s: *v(36),
	}
	caps := map[inventory.Capability]inventory.CapabilityStatus{}
	for _, c := range []inventory.Capability{inventory.CapAPIUsage, inventory.CapDeprecatedCalls, inventory.CapHelm, inventory.CapAddOns, inventory.CapVersions, inventory.CapCRDs} {
		caps[c] = inventory.CapabilityStatus{Available: true}
	}
	inv := inventory.Inventory{
		Source: inventory.SourceCluster, ServerVersion: "v1.31.4", Capabilities: caps,
		Nodes: []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.31.4"}},
		APIUsage: []inventory.APIUsage{{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Count: 1,
			Namespaces: map[string]int{"": 1}, Objects: []inventory.ObjectRef{obj}}},
		DeprecatedCalls: []inventory.DeprecatedCall{{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Resource: "flowschemas", RemovedRelease: "1.32"}},
	}
	r := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 32}, now)
	if r.Verdict != engine.VerdictBlocked || len(r.Findings) != 1 || r.Findings[0].Key != flowSchemaKey || len(r.Findings[0].Callers) != 1 {
		t.Fatalf("precondition: verdict %s, findings %+v; want one blocker carrying the caller", r.Verdict, r.Findings)
	}
	return r
}

// #236: suppressing every object of a removed-api finding leaves the
// caller folded into it standing, as the deprecated-api-in-use finding it
// is on its own: the verdict stays blocked and names the caller. The
// suppressed entry keeps its objects, without the caller.
func TestApplyObjectSuppressionKeepsFoldedCallers(t *testing.T) {
	legacy := inventory.ObjectRef{Name: "legacy-fs", Manager: "helm", File: "apf/flowschema.yaml", Line: 1}
	annotated := legacy
	annotated.Ignore, annotated.IgnoreReason = "removed-api", "deleting next sprint"
	cases := []struct {
		name  string
		obj   inventory.ObjectRef
		rules []Rule
	}{
		{"annotation", annotated, nil},
		{"name rule", legacy, []Rule{{Key: flowSchemaKey, Name: "legacy-fs", Reason: "deleting next sprint"}}},
		{"file rule", legacy, []Rule{{Category: "removed-api", File: "apf/*.yaml", Reason: "deleting next sprint"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := callerReport(t, tc.obj)
			want := r.Findings[0].Callers[0].Finding()
			got, warnings := Apply(r, tc.rules, Options{Now: now, Source: ".upgradescope.yaml"})
			if len(warnings) != 0 {
				t.Errorf("warnings = %v", warnings)
			}
			if got.Verdict != engine.VerdictBlocked {
				t.Errorf("verdict = %s, want blocked by the caller", got.Verdict)
			}
			if !reflect.DeepEqual(got.Findings, []engine.Finding{want}) {
				t.Errorf("findings = %+v\nwant %+v", got.Findings, []engine.Finding{want})
			}
			if len(got.Suppressed) != 1 || got.Suppressed[0].Key != flowSchemaKey || len(got.Suppressed[0].Objects) != 1 || got.Suppressed[0].Callers != nil {
				t.Errorf("suppressed = %+v, want the object finding without its callers", got.Suppressed)
			}
		})
	}
}

// Only a rule without object selectors, or one for the caller's own
// deprecated-api-in-use key or category, suppresses the caller.
func TestApplyWholeFindingRulesSuppressCallers(t *testing.T) {
	annotated := inventory.ObjectRef{Name: "legacy-fs", Manager: "helm", Ignore: "removed-api", IgnoreReason: "deleting next sprint"}
	cases := []struct {
		name       string
		obj        inventory.ObjectRef
		rules      []Rule
		suppressed []string // keys, in order
		callers    []int    // number of callers on each suppressed entry
	}{
		{"key rule", inventory.ObjectRef{Name: "legacy-fs"}, []Rule{{Key: flowSchemaKey, Reason: "accepted"}}, []string{flowSchemaKey}, []int{1}},
		{"category rule", inventory.ObjectRef{Name: "legacy-fs"}, []Rule{{Category: "removed-api", Reason: "accepted"}}, []string{flowSchemaKey}, []int{1}},
		{"annotation and caller key", annotated, []Rule{{Key: callerKey, Reason: "the caller is helm"}}, []string{flowSchemaKey, callerKey}, []int{0, 0}},
		{"annotation and caller category", annotated, []Rule{{Category: "deprecated-api-in-use", Reason: "the caller is helm"}}, []string{flowSchemaKey, callerKey}, []int{0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Apply(callerReport(t, tc.obj), tc.rules, Options{Now: now})
			if len(got.Findings) != 0 || got.Verdict != engine.VerdictReady {
				t.Errorf("verdict %s, findings %+v; want ready, none", got.Verdict, got.Findings)
			}
			var keys []string
			var callers []int
			for _, s := range got.Suppressed {
				keys, callers = append(keys, s.Key), append(callers, len(s.Callers))
			}
			if !slices.Equal(keys, tc.suppressed) || !slices.Equal(callers, tc.callers) {
				t.Errorf("suppressed %q with %v callers, want %q with %v", keys, callers, tc.suppressed, tc.callers)
			}
		})
	}
}

// A rule that takes some objects leaves the finding, and its callers, in
// place.
func TestApplyPartialObjectSuppressionKeepsCallersOnFinding(t *testing.T) {
	r := callerReport(t, inventory.ObjectRef{Name: "legacy-fs"})
	r.Findings[0].Objects = append(r.Findings[0].Objects, inventory.ObjectRef{Name: "other-fs"})
	got, _ := Apply(r, []Rule{{Key: flowSchemaKey, Name: "legacy-fs", Reason: "deleting"}}, Options{Now: now})
	if len(got.Findings) != 1 || got.Findings[0].Key != flowSchemaKey || len(got.Findings[0].Callers) != 1 {
		t.Fatalf("findings = %+v, want the object finding with its caller", got.Findings)
	}
	if len(got.Suppressed) != 1 || got.Suppressed[0].Callers != nil {
		t.Errorf("suppressed = %+v, want one entry without callers", got.Suppressed)
	}
}

// A re-emitted caller lists no objects or namespaces, so a rule for its
// key or category that also has object selectors matches nothing: the
// caller still counts.
func TestApplyCallerRuleWithSelectorsTakesNothing(t *testing.T) {
	annotated := inventory.ObjectRef{Name: "legacy-fs", Manager: "helm", Ignore: "removed-api", IgnoreReason: "deleting next sprint"}
	for _, rule := range []Rule{
		{Key: callerKey, Namespace: "*", Reason: "the caller is helm"},
		{Key: callerKey, Name: "legacy-fs", Reason: "the caller is helm"},
		{Category: "deprecated-api-in-use", File: "*", Reason: "the caller is helm"},
	} {
		got, _ := Apply(callerReport(t, annotated), []Rule{rule}, Options{Now: now})
		if got.Verdict != engine.VerdictBlocked || len(got.Findings) != 1 || got.Findings[0].Key != callerKey {
			t.Errorf("rule %+v: verdict %s, findings %+v; want blocked by the caller", rule, got.Verdict, got.Findings)
		}
		if len(got.Suppressed) != 1 || got.Suppressed[0].Key != flowSchemaKey {
			t.Errorf("rule %+v: suppressed = %+v, want only the annotated object finding", rule, got.Suppressed)
		}
	}
}

// A re-emitted caller is judged after every finding of the report, so
// Apply re-sorts: findings[] keeps the report's order (severity first,
// engine.SortFindings), and the blocker caller leads the report's warning
// and info.
func TestApplyReemittedCallerKeepsSeverityOrder(t *testing.T) {
	annotated := inventory.ObjectRef{Name: "legacy-fs", Manager: "helm", Ignore: "removed-api", IgnoreReason: "deleting next sprint"}
	r := callerReport(t, annotated)
	caller := r.Findings[0].Callers[0].Finding()
	if caller.Severity != engine.SevBlocker {
		t.Fatalf("precondition: caller severity %s, want blocker", caller.Severity)
	}
	warning := engine.Finding{Category: engine.CatDeprecatedAPI, Severity: engine.SevWarning, Key: "deprecated-api/example.com/v1beta1/Widget", Title: "example.com/v1beta1 Widget is deprecated"}
	info := engine.Finding{Category: engine.CatEOLApproaching, Severity: engine.SevInfo, Key: "eol-approaching/example", Title: "example approaches end of life"}
	r.Findings = append(r.Findings, warning, info)

	got, _ := Apply(r, nil, Options{Now: now})
	if want := []engine.Finding{caller, warning, info}; !reflect.DeepEqual(got.Findings, want) {
		var keys []string
		for _, f := range got.Findings {
			keys = append(keys, f.Key)
		}
		t.Errorf("findings %q, want %q, %q, %q (blocker first)", keys, caller.Key, warning.Key, info.Key)
	}
	if got.Verdict != engine.VerdictBlocked {
		t.Errorf("verdict = %s, want blocked by the caller", got.Verdict)
	}
}
