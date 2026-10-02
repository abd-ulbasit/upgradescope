package engine

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func callsInv(c inventory.DeprecatedCall) inventory.Inventory {
	return inventory.Inventory{DeprecatedCalls: []inventory.DeprecatedCall{c}}
}

func TestEvalDeprecatedCallsSeverityVsTarget(t *testing.T) {
	row := inventory.DeprecatedCall{Group: "batch", Version: "v1beta1", Resource: "cronjobs", RemovedRelease: "1.25"}
	cases := []struct {
		name   string
		target inventory.Version
		sev    Severity
	}{
		{"removed at target", inventory.Version{Major: 1, Minor: 25}, SevBlocker},
		{"removed before target", inventory.Version{Major: 1, Minor: 26}, SevBlocker},
		{"removed at target+1", inventory.Version{Major: 1, Minor: 24}, SevWarning},
		{"removed beyond window", inventory.Version{Major: 1, Minor: 23}, SevInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := evalDeprecatedCalls(callsInv(row), tc.target)
			if len(fs) != 1 || fs[0].Severity != tc.sev || fs[0].Category != CatDeprecatedAPIInUse {
				t.Fatalf("want one %s deprecated-api-in-use, got %+v", tc.sev, fs)
			}
			if fs[0].Title != "clients still requesting batch/v1beta1 cronjobs (removed in 1.25)" {
				t.Fatalf("title = %q", fs[0].Title)
			}
			if fs[0].Key != "deprecated-api-in-use/batch/v1beta1/cronjobs" {
				t.Fatalf("key = %q", fs[0].Key)
			}
		})
	}
}

func TestEvalDeprecatedCallsUnparseableIsInfo(t *testing.T) {
	row := inventory.DeprecatedCall{Group: "extensions", Version: "v1beta1", Resource: "ingresses"}
	fs := evalDeprecatedCalls(callsInv(row), inventory.Version{Major: 1, Minor: 34})
	if len(fs) != 1 || fs[0].Severity != SevInfo {
		t.Fatalf("missing removedRelease must yield info, got %+v", fs)
	}
	if fs[0].Title != "clients still requesting extensions/v1beta1 ingresses (deprecated)" {
		t.Fatalf("title = %q", fs[0].Title)
	}
	if fs[0].Key != "deprecated-api-in-use/extensions/v1beta1/ingresses" {
		t.Fatalf("key = %q", fs[0].Key)
	}
	if fs[0].Detail != "apiserver_requested_deprecated_apis records requests to this API since the last apiserver restart; no removal release is recorded. The metric does not identify the client; apiserver audit logs do." {
		t.Fatalf("detail = %q", fs[0].Detail)
	}
}

func TestEvalDeprecatedCallsUnparseableReleaseIsInfo(t *testing.T) {
	row := inventory.DeprecatedCall{Group: "extensions", Version: "v1beta1", Resource: "ingresses", RemovedRelease: "soon"}
	fs := evalDeprecatedCalls(callsInv(row), inventory.Version{Major: 1, Minor: 34})
	if len(fs) != 1 || fs[0].Severity != SevInfo {
		t.Fatalf("unparseable removedRelease must yield info, got %+v", fs)
	}
	if fs[0].Title != "clients still requesting extensions/v1beta1 ingresses (deprecated)" {
		t.Fatalf("title = %q", fs[0].Title)
	}
	if fs[0].Detail != `apiserver_requested_deprecated_apis records requests to this API since the last apiserver restart; removal release "soon" could not be parsed. The metric does not identify the client; apiserver audit logs do.` {
		t.Fatalf("detail = %q", fs[0].Detail)
	}
}

func TestEvalDeprecatedCallsSubresource(t *testing.T) {
	row := inventory.DeprecatedCall{Group: "apps", Version: "v1beta2", Resource: "deployments",
		Subresource: "scale", RemovedRelease: "1.16"}
	fs := evalDeprecatedCalls(callsInv(row), inventory.Version{Major: 1, Minor: 34})
	if len(fs) != 1 || fs[0].Title != "clients still requesting apps/v1beta2 deployments/scale (removed in 1.16)" {
		t.Fatalf("got %+v", fs)
	}
	if fs[0].Key != "deprecated-api-in-use/apps/v1beta2/deployments/scale" {
		t.Fatalf("key = %q", fs[0].Key)
	}
}

// Issue #29's repro: one removed API with both a written object and a
// caller row must be scored once, with both pieces of evidence.
func TestEvaluateMergesCallersIntoAPIUsageFinding(t *testing.T) {
	inv := inventory.Inventory{
		ServerVersion: "v1.24.9",
		APIUsage: []inventory.APIUsage{{
			Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 1,
			Namespaces: map[string]int{"default": 1},
			Objects:    []inventory.ObjectRef{{Namespace: "default", Name: "nightly", Manager: "kubectl-client-side-apply"}},
		}},
		DeprecatedCalls: []inventory.DeprecatedCall{{Group: "batch", Version: "v1beta1", Resource: "cronjobs", RemovedRelease: "1.25"}},
	}
	rep := Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 25}, testNow)
	if len(rep.Findings) != 1 {
		t.Fatalf("want exactly one finding, got %+v", rep.Findings)
	}
	f := rep.Findings[0]
	if f.Severity != SevBlocker || f.Key != "removed-api/batch/v1beta1/CronJob" {
		t.Errorf("finding = %s %s, want blocker removed-api/batch/v1beta1/CronJob", f.Severity, f.Key)
	}
	want := "1 object(s) written through this API version: default (1). Written by: kubectl-client-side-apply. " +
		"apiserver_requested_deprecated_apis also records requests to batch/v1beta1 cronjobs since the last apiserver restart; the metric does not identify the client."
	if f.Detail != want {
		t.Errorf("detail = %q\nwant     %q", f.Detail, want)
	}
	if rep.Score != 75 {
		t.Errorf("score = %d, want 75 (one blocker)", rep.Score)
	}
}

func TestEvaluateMergesSubresourceCallers(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{Group: "extensions", Version: "v1beta1", Kind: "Ingress", Count: 1, Namespaces: map[string]int{"shop": 1}}},
		DeprecatedCalls: []inventory.DeprecatedCall{
			{Group: "extensions", Version: "v1beta1", Resource: "ingresses", RemovedRelease: "1.22"},
			{Group: "extensions", Version: "v1beta1", Resource: "ingresses", Subresource: "status", RemovedRelease: "1.22"},
		},
	}
	rep := Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 22}, testNow)
	if len(rep.Findings) != 1 {
		t.Fatalf("want exactly one finding, got %+v", rep.Findings)
	}
	if !strings.Contains(rep.Findings[0].Detail, "requests to extensions/v1beta1 ingresses, ingresses/status since") {
		t.Errorf("detail = %q, want both caller rows as evidence", rep.Findings[0].Detail)
	}
}

func TestEvaluateCallerWithoutObjectsStaysStandalone(t *testing.T) {
	inv := inventory.Inventory{
		DeprecatedCalls: []inventory.DeprecatedCall{{Group: "batch", Version: "v1beta1", Resource: "cronjobs", RemovedRelease: "1.25"}},
	}
	rep := Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 25}, testNow)
	if len(rep.Findings) != 1 || rep.Findings[0].Key != "deprecated-api-in-use/batch/v1beta1/cronjobs" || rep.Findings[0].Severity != SevBlocker {
		t.Fatalf("want one standalone deprecated-api-in-use blocker, got %+v", rep.Findings)
	}
}

// A caller row more severe than the KB entry (the apiserver records a
// removal the KB does not know) is not folded into a lesser finding.
func TestEvaluateMoreSevereCallerIsNotFolded(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage:        []inventory.APIUsage{{Version: "v1", Kind: "ComponentStatus", Count: 1, Namespaces: map[string]int{"": 1}}},
		DeprecatedCalls: []inventory.DeprecatedCall{{Version: "v1", Resource: "componentstatuses", RemovedRelease: "1.30"}},
	}
	rep := Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 34}, testNow)
	if len(rep.Findings) != 2 {
		t.Fatalf("want the info usage finding and the blocker caller finding, got %+v", rep.Findings)
	}
}

func TestEvaluateFoldsCoreAndIrregularPlurals(t *testing.T) {
	k := testKB()
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Version: "v1", Kind: "Endpoints", Introduced: inventory.Version{Major: 1}, Deprecated: vp(1, 33),
	})
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{
			{Version: "v1", Kind: "ComponentStatus", Count: 1, Namespaces: map[string]int{"": 1}},
			{Version: "v1", Kind: "Endpoints", Count: 1, Namespaces: map[string]int{"default": 1}},
		},
		DeprecatedCalls: []inventory.DeprecatedCall{
			{Version: "v1", Resource: "componentstatuses"},
			{Version: "v1", Resource: "endpoints"},
		},
	}
	rep := Evaluate(inv, k, inventory.Version{Major: 1, Minor: 34}, testNow)
	if len(rep.Findings) != 2 {
		t.Fatalf("want each caller row folded into its usage finding, got %+v", rep.Findings)
	}
	for _, f := range rep.Findings {
		if f.Category != CatDeprecatedAPI || !strings.Contains(f.Detail, "apiserver_requested_deprecated_apis") {
			t.Errorf("finding %s: detail %q lacks caller evidence", f.Key, f.Detail)
		}
	}
}
