package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func apiFindingFor(t *testing.T, r Report, wantInTitle string) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if (f.Category == CatRemovedAPI || f.Category == CatDeprecatedAPI) && strings.Contains(f.Title, wantInTitle) {
			return f
		}
	}
	t.Fatalf("no api finding with %q in %+v", wantInTitle, r.Findings)
	return Finding{}
}

// #300: the "not served until X" blocker carries its phase in the key, so a
// rule or baseline for it is not a rule or baseline for the removal that
// follows; the removal blocker and the next-minor warning keep the key
// existing rules use, which an ignore rule accepts whatever the severity.
func TestUnservedBlockerHasItsOwnKey(t *testing.T) {
	k := realKB(t)
	sc := manifestUsage("networking.k8s.io", "v1beta1", "ServiceCIDR") // served from 1.31, removed in 1.37
	for _, c := range []struct {
		target   int
		sev      Severity
		title    string
		wantKey  string
		wantCate Category
	}{
		{30, SevBlocker, "is not served until 1.31", "removed-api/networking.k8s.io/v1beta1/ServiceCIDR/unserved", CatRemovedAPI},
		{36, SevWarning, "removed in 1.37", "removed-api/networking.k8s.io/v1beta1/ServiceCIDR", CatRemovedAPI},
		{37, SevBlocker, "removed in 1.37", "removed-api/networking.k8s.io/v1beta1/ServiceCIDR", CatRemovedAPI},
	} {
		r := Evaluate(manifestsInv(sc), k, inventory.Version{Major: 1, Minor: c.target}, testNow)
		f := apiFindingFor(t, r, c.title)
		if f.Key != c.wantKey || f.Severity != c.sev || f.Category != c.wantCate {
			t.Errorf("target 1.%d: %s %s key %q; want %s %s key %q", c.target, f.Category, f.Severity, f.Key, c.wantCate, c.sev, c.wantKey)
		}
	}
}

// UnservedKey is the one place the suffix is spelled.
func TestUnservedKeySuffix(t *testing.T) {
	base := "removed-api/networking.k8s.io/v1beta1/ServiceCIDR"
	if got := UnservedKey(base); got != base+"/unserved" {
		t.Errorf("UnservedKey = %q", got)
	}
	if b, ok := BaseOfUnservedKey(base + "/unserved"); !ok || b != base {
		t.Errorf("BaseOfUnservedKey = %q %v", b, ok)
	}
	if _, ok := BaseOfUnservedKey(base); ok {
		t.Error("a bare key is not an unserved key")
	}
}

// A caller row for the API of an unserved finding still folds into it, and
// a partial api-usage gap that skipped the API still hides it: both match
// the API in the key, not the phase after it.
func TestUnservedKeyStillMatchesItsAPI(t *testing.T) {
	usage := "removed-api/resource.k8s.io/v1/DeviceClass/unserved"
	call := "deprecated-api-in-use/resource.k8s.io/v1/deviceclasses"
	if !FoldsInto(call, usage) {
		t.Errorf("FoldsInto(%q, %q) = false", call, usage)
	}
	gaps := []CapabilityGap{{Capability: inventory.CapAPIUsage, Partial: true, Skipped: []string{"resource.k8s.io/v1 DeviceClass"}}}
	if got := HiddenBy(gaps, CatRemovedAPI, usage); !slices.Equal(got, []inventory.Capability{inventory.CapAPIUsage}) {
		t.Errorf("HiddenBy = %v, want api-usage", got)
	}
	// and a different API is not hidden
	if got := HiddenBy(gaps, CatRemovedAPI, "removed-api/resource.k8s.io/v1/DeviceClass2/unserved"); len(got) != 0 {
		t.Errorf("HiddenBy other kind = %v", got)
	}
}

func TestDeprecatedCallFoldsIntoUnservedFinding(t *testing.T) {
	k := realKB(t)
	dc := manifestUsage("resource.k8s.io", "v1", "DeviceClass")
	inv := manifestsInv(dc)
	inv.DeprecatedCalls = []inventory.DeprecatedCall{{Group: "resource.k8s.io", Version: "v1", Resource: "deviceclasses", RemovedRelease: "1.33"}}
	r := Evaluate(inv, k, inventory.Version{Major: 1, Minor: 33}, testNow)
	f := apiFindingFor(t, r, "is not served until 1.34")
	if len(f.Callers) != 1 {
		t.Errorf("callers = %+v, want the call folded into the unserved finding %s", f.Callers, f.Key)
	}
}

// #302: a removed alpha whose kind has only a later non-GA version names
// that version, with its stability.
func TestRemovedAlphaRemediatesToTheServedSuccessor(t *testing.T) {
	k := realKB(t)
	for _, c := range []struct {
		name         string
		group, v, kd string
		target       int
		sev          Severity
		want         string
	}{
		{"Workload v1alpha2 removed at 1.37", "scheduling.k8s.io", "v1alpha2", "Workload", 37, SevBlocker, "migrate to scheduling.k8s.io/v1beta1 Workload (beta; may need enabling)"},
		{"PodGroup v1alpha2 removed at 1.37", "scheduling.k8s.io", "v1alpha2", "PodGroup", 37, SevBlocker, "migrate to scheduling.k8s.io/v1beta1 PodGroup (beta; may need enabling)"},
		{"LeaseCandidate v1alpha2 warned at 1.37", "coordination.k8s.io", "v1alpha2", "LeaseCandidate", 37, SevWarning, "migrate to coordination.k8s.io/v1beta1 LeaseCandidate (beta; may need enabling)"},
		// v1beta1 is served from 1.33, after this removal: the newest served then is an alpha.
		{"LeaseCandidate v1alpha1 removed at 1.32", "coordination.k8s.io", "v1alpha1", "LeaseCandidate", 32, SevBlocker, "migrate to coordination.k8s.io/v1alpha2 LeaseCandidate (alpha; must be enabled; itself removed in 1.38)"},
		{"Workload v1alpha1 removed at 1.36", "scheduling.k8s.io", "v1alpha1", "Workload", 36, SevBlocker, "migrate to scheduling.k8s.io/v1alpha2 Workload (alpha; must be enabled; itself removed in 1.37)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := Evaluate(manifestsInv(manifestUsage(c.group, c.v, c.kd)), k, inventory.Version{Major: 1, Minor: c.target}, testNow)
			var f Finding
			for _, g := range r.Findings {
				if g.Category == CatRemovedAPI {
					f = g
				}
			}
			if f.Severity != c.sev {
				t.Fatalf("findings = %+v, want a removed-api %s", r.Findings, c.sev)
			}
			if !strings.HasPrefix(f.Remediation, c.want) {
				t.Errorf("remediation = %q, want it to start %q", f.Remediation, c.want)
			}
			if f.Key != "removed-api/"+apiKey(c.group, c.v, c.kd) {
				t.Errorf("key = %q: the removal keeps the bare key", f.Key)
			}
		})
	}
}

// A successor that is itself removed later says so.
func TestAlphaSuccessorSaysWhenItIsRemovedLater(t *testing.T) {
	k := realKB(t)
	r := Evaluate(manifestsInv(manifestUsage("coordination.k8s.io", "v1alpha1", "LeaseCandidate")), k, inventory.Version{Major: 1, Minor: 32}, testNow)
	f := apiFindingFor(t, r, "removed in 1.32")
	if !strings.Contains(f.Remediation, "removed in 1.38") {
		t.Errorf("remediation = %q, want it to say v1alpha2 is removed in 1.38", f.Remediation)
	}
}

// A replacement chain still wins over the fallback, and a deprecated entry
// with nothing later gets none.
func TestSuccessorFallbackDoesNotReplaceTheChain(t *testing.T) {
	k := realKB(t)
	r := Evaluate(manifestsInv(manifestUsage("batch", "v1beta1", "CronJob")), k, inventory.Version{Major: 1, Minor: 25}, testNow)
	if f := apiFindingFor(t, r, "removed in 1.25"); f.Remediation != "migrate to batch/v1 CronJob" {
		t.Errorf("remediation = %q", f.Remediation)
	}
}

// Helm releases get the same fallback: a stored manifest at a removed alpha.
func TestHelmManifestRemediatesToTheServedSuccessor(t *testing.T) {
	k := realKB(t)
	rel := inventory.HelmRelease{
		Name: "sched", Namespace: "platform", ChartName: "sched", ChartVersion: "0.1.0", Status: "deployed", Revision: 1,
		ManifestAPIs: []inventory.APIUsage{{Group: "scheduling.k8s.io", Version: "v1alpha2", Kind: "Workload", Count: 1,
			Namespaces: map[string]int{"platform": 1}, Objects: []inventory.ObjectRef{{Name: "w", Namespace: "platform", Line: 3}}}},
	}
	fs := evalHelmManifest(rel, kb.NewIndex(k.APILifecycle), nil, inventory.Version{Major: 1, Minor: 37}, k.MaxKnownK8s)
	if len(fs) != 1 || !strings.Contains(fs[0].Remediation, "(scheduling.k8s.io/v1beta1 Workload (beta; may need enabling))") {
		t.Errorf("findings = %+v, want the remediation to name v1beta1 Workload (beta; may need enabling)", fs)
	}
}
