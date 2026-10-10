package engine

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func shippedKB(t testing.TB) kb.KB {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatalf("kb.Load: %v", err)
	}
	return k
}

func callerFindings(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Category == CatDeprecatedAPIInUse {
			out = append(out, f)
		}
	}
	return out
}

// #236: the apiserver labels a row with the upstream lifecycle tag, but
// the knowledge base records the earlier release kube-apiserver stopped
// serving an inferred removal in. StorageVersionMigration v1alpha1 is
// tagged removed in 1.36 and gone from 1.35.
func TestEvalDeprecatedCallsJudgedByKBRemoval(t *testing.T) {
	k := shippedKB(t)
	e, ok := kb.NewIndex(k.APILifecycle).Lookup("storagemigration.k8s.io", "v1alpha1", "StorageVersionMigration")
	if !ok || e.Removed == nil || *e.Removed != (inventory.Version{Major: 1, Minor: 35}) {
		t.Fatalf("precondition: KB entry %+v, want removed 1.35", e)
	}
	inv := clusterInv()
	inv.DeprecatedCalls = []inventory.DeprecatedCall{{Group: "storagemigration.k8s.io", Version: "v1alpha1", Resource: "storageversionmigrations", RemovedRelease: "1.36"}}
	rep := Evaluate(inv, k, inventory.Version{Major: 1, Minor: 35}, testNow)
	calls := callerFindings(rep.Findings)
	if len(calls) != 1 || calls[0].Severity != SevBlocker {
		t.Fatalf("callers = %+v, want one blocker", calls)
	}
	if rep.Verdict != VerdictBlocked {
		t.Errorf("verdict = %s, want blocked", rep.Verdict)
	}
	f := calls[0]
	if f.Title != "clients still requesting storagemigration.k8s.io/v1alpha1 storageversionmigrations (removed in 1.35)" {
		t.Errorf("title = %q", f.Title)
	}
	if !strings.Contains(f.Detail, "it is removed in 1.35 per the knowledge base; the apiserver reports 1.36.") {
		t.Errorf("detail = %q, want the KB named as the source", f.Detail)
	}
}

// The issue's stronger example: IPAddress v1alpha1 is tagged removed in
// 1.33 but deleted in 1.31, so a caller at target 1.31 blocks.
func TestEvalDeprecatedCallsIPAddressKBRemoval(t *testing.T) {
	k := shippedKB(t)
	row := inventory.DeprecatedCall{Group: "networking.k8s.io", Version: "v1alpha1", Resource: "ipaddresses", RemovedRelease: "1.33"}
	fs := evalDeprecatedCalls(callsInv(row), k, inventory.Version{Major: 1, Minor: 31}, nil)
	if len(fs) != 1 || fs[0].Severity != SevBlocker {
		t.Fatalf("got %+v, want one blocker", fs)
	}
}

// A row whose label is later than the KB is judged by the KB; one the KB
// does not know keeps the label; a KB removal with no label still counts;
// a KB entry without a removal leaves the label in charge.
func TestEvalDeprecatedCallsRemovalSource(t *testing.T) {
	k := testKB()
	cases := []struct {
		name   string
		row    inventory.DeprecatedCall
		target int
		sev    Severity
		title  string
		detail string
	}{
		{"label later than KB", inventory.DeprecatedCall{Group: "batch", Version: "v1beta1", Resource: "cronjobs", RemovedRelease: "1.27"}, 25, SevBlocker,
			"clients still requesting batch/v1beta1 cronjobs (removed in 1.25)", "it is removed in 1.25 per the knowledge base; the apiserver reports 1.27."},
		{"label agrees", inventory.DeprecatedCall{Group: "batch", Version: "v1beta1", Resource: "cronjobs", RemovedRelease: "1.25"}, 24, SevWarning,
			"clients still requesting batch/v1beta1 cronjobs (removed in 1.25)", "it is removed in 1.25."},
		{"no label", inventory.DeprecatedCall{Group: "batch", Version: "v1beta1", Resource: "cronjobs"}, 25, SevBlocker,
			"clients still requesting batch/v1beta1 cronjobs (removed in 1.25)", "it is removed in 1.25 per the knowledge base; the apiserver records no removal release."},
		{"unparseable label", inventory.DeprecatedCall{Group: "batch", Version: "v1beta1", Resource: "cronjobs", Subresource: "status", RemovedRelease: "soon"}, 25, SevBlocker,
			"clients still requesting batch/v1beta1 cronjobs/status (removed in 1.25)", `it is removed in 1.25 per the knowledge base; the apiserver's removal release "soon" could not be parsed.`},
		{"not in the KB", inventory.DeprecatedCall{Group: "policy", Version: "v1beta1", Resource: "podsecuritypolicies", RemovedRelease: "1.25"}, 24, SevWarning,
			"clients still requesting policy/v1beta1 podsecuritypolicies (removed in 1.25)", "it is removed in 1.25."},
		{"KB without a removal", inventory.DeprecatedCall{Version: "v1", Resource: "componentstatuses", RemovedRelease: "1.30"}, 34, SevBlocker,
			"clients still requesting v1 componentstatuses (removed in 1.30)", "it is removed in 1.30."},
		// The KB wins when it is later too: the label alone would block.
		{"KB later than label", inventory.DeprecatedCall{Group: "example.com", Version: "v1beta1", Resource: "widgets", RemovedRelease: "1.25"}, 26, SevWarning,
			"clients still requesting example.com/v1beta1 widgets (removed in 1.27)", "it is removed in 1.27 per the knowledge base; the apiserver reports 1.25."},
	}
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{Group: "example.com", Version: "v1beta1", Kind: "Widget",
		Introduced: inventory.Version{Major: 1, Minor: 20}, Deprecated: vp(1, 24), Removed: vp(1, 27)})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := evalDeprecatedCalls(callsInv(tc.row), k, inventory.Version{Major: 1, Minor: tc.target}, nil)
			if len(fs) != 1 {
				t.Fatalf("got %+v", fs)
			}
			if fs[0].Severity != tc.sev || fs[0].Title != tc.title || !strings.Contains(fs[0].Detail, tc.detail) {
				t.Errorf("got %s %q %q\nwant %s %q containing %q", fs[0].Severity, fs[0].Title, fs[0].Detail, tc.sev, tc.title, tc.detail)
			}
		})
	}
}

// A folded caller row is kept on the finding as structured data: what
// its standalone finding would have been.
func TestFoldKeepsCallersAsData(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 1, Namespaces: map[string]int{"default": 1},
			Objects: []inventory.ObjectRef{{Namespace: "default", Name: "nightly", Manager: "helm"}}}},
		DeprecatedCalls: []inventory.DeprecatedCall{
			{Group: "batch", Version: "v1beta1", Resource: "cronjobs", Subresource: "status", RemovedRelease: "1.25"},
			{Group: "batch", Version: "v1beta1", Resource: "cronjobs", RemovedRelease: "1.25"},
		},
	}
	target := inventory.Version{Major: 1, Minor: 25}
	rep := Evaluate(inv, testKB(), target, testNow)
	if len(rep.Findings) != 1 {
		t.Fatalf("findings = %+v", rep.Findings)
	}
	standalone := Evaluate(inventory.Inventory{DeprecatedCalls: inv.DeprecatedCalls}, testKB(), target, testNow).Findings
	var want []Caller
	for _, key := range []string{"deprecated-api-in-use/batch/v1beta1/cronjobs", "deprecated-api-in-use/batch/v1beta1/cronjobs/status"} { // row order
		f := standaloneByKey(standalone, key)
		want = append(want, Caller{Key: f.Key, Severity: f.Severity, Title: f.Title, Detail: f.Detail})
	}
	want[0].Group, want[0].Version, want[0].Resource = "batch", "v1beta1", "cronjobs"
	want[1].Group, want[1].Version, want[1].Resource, want[1].Subresource = "batch", "v1beta1", "cronjobs", "status"
	if got := rep.Findings[0].Callers; !reflect.DeepEqual(got, want) {
		t.Errorf("callers = %+v\nwant %+v", got, want)
	}
	for i, c := range rep.Findings[0].Callers {
		if got := c.Finding(); !reflect.DeepEqual(got, standaloneByKey(standalone, c.Key)) {
			t.Errorf("caller %d Finding() = %+v\nwant %+v", i, got, standaloneByKey(standalone, c.Key))
		}
	}
}

// A folded caller row is charged at least what it takes on the finding:
// its Caller repeats the requested group, version, resource and
// subresource, which its standalone finding's charge did not count. With
// a 16 KiB subresource (the ingest limit), the report used to be charged
// about three quarters of what findingSize says it takes, so
// EvaluateWithin let it run that far over its limit.
func TestEvaluateWithinChargesFoldedCallers(t *testing.T) {
	long := strings.Repeat("s", 16<<10)
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 1, Namespaces: map[string]int{"default": 1},
			Objects: []inventory.ObjectRef{{Namespace: "default", Name: "nightly"}}}},
		DeprecatedCalls: []inventory.DeprecatedCall{
			{Group: "batch", Version: "v1beta1", Resource: "cronjobs", Subresource: long, RemovedRelease: "1.25"},
			{Group: "batch", Version: "v1beta1", Resource: "cronjobs", Subresource: long + "2", RemovedRelease: "1.25"},
		},
	}
	k := testKB()
	target := inventory.Version{Major: 1, Minor: 25}
	want := Evaluate(inv, k, target, testNow)
	if len(want.Findings) != 1 || len(want.Findings[0].Callers) != 2 {
		t.Fatalf("precondition: findings = %d, want one with two callers", len(want.Findings))
	}
	charged := reportBaseSize(inv, k)
	for i := range want.Findings {
		charged += findingSize(&want.Findings[i])
	}
	for i := range want.NotAssessed {
		charged += gapSize(&want.NotAssessed[i])
	}
	if _, err := EvaluateWithin(inv, k, target, testNow, charged-1); !errors.Is(err, ErrReportTooLarge) {
		t.Errorf("EvaluateWithin(%d bytes, one under what the report's findings take) = %v, want ErrReportTooLarge", charged-1, err)
	}
	if got, err := EvaluateWithin(inv, k, target, testNow, 2*charged); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("EvaluateWithin(2 × %d bytes) = %v, want Evaluate's report", charged, err)
	}
}

func standaloneByKey(fs []Finding, key string) Finding {
	for _, f := range fs {
		if f.Key == key {
			return f
		}
	}
	return Finding{}
}

// #236: a Helm manifest's deprecated-API warning is not swallowed by the
// live scan's info finding for the same object; more evidence never
// lowers the severity.
func TestHelmManifestWarningSurvivesLiveInfo(t *testing.T) {
	inv := clusterInv()
	inv.ServerVersion = "v1.29.3"
	inv.Nodes = []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.29.3"}}
	inv.HelmReleases = []inventory.HelmRelease{flowSchemaRelease()}
	target := inventory.Version{Major: 1, Minor: 30}
	without := Evaluate(inv, helmTestKB(), target, testNow)
	inv.APIUsage = []inventory.APIUsage{{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Count: 1,
		Namespaces: map[string]int{"": 1}, Objects: []inventory.ObjectRef{{Name: "batch-jobs", Manager: "helm"}}}}
	with := Evaluate(inv, helmTestKB(), target, testNow)
	if without.Score != 95 {
		t.Fatalf("precondition: score without the live object = %d, want 95", without.Score)
	}
	if with.Score != without.Score {
		t.Errorf("score with the live object = %d, want %d: more evidence must not raise the score", with.Score, without.Score)
	}
	var keys []string
	for _, f := range with.Findings {
		keys = append(keys, string(f.Severity)+" "+f.Key)
	}
	want := []string{"warning deprecated-api/helm-release/platform/apf", "info deprecated-api/flowcontrol.apiserver.k8s.io/v1beta3/FlowSchema"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("findings = %q, want %q", keys, want)
	}
}

// At target+1 removal the live finding is a removed-api warning, as
// severe as the manifest's deprecated-api warning: the object is counted
// once, by the live finding.
func TestHelmManifestDedupesEquallySevereLive(t *testing.T) {
	inv := inventory.Inventory{
		HelmReleases: []inventory.HelmRelease{flowSchemaRelease()},
		APIUsage: []inventory.APIUsage{{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Count: 1,
			Namespaces: map[string]int{"": 1}, Objects: []inventory.ObjectRef{{Name: "batch-jobs", Manager: "helm"}}}},
	}
	rep := Evaluate(inv, helmTestKB(), inventory.Version{Major: 1, Minor: 31}, testNow)
	if keys := findingKeys(rep.Findings); !reflect.DeepEqual(keys, []string{"removed-api/flowcontrol.apiserver.k8s.io/v1beta3/FlowSchema"}) {
		t.Errorf("findings = %q, want only the live removed-api warning", keys)
	}
}

// foldInventory is n usage entries of group g/v kind K<i> and n caller
// rows of resource r<i>: no row folds, the worst case for a scan of
// every usage entry per row.
func foldInventory(n int) inventory.Inventory {
	inv := inventory.Inventory{SchemaVersion: 1, ServerVersion: "1.30"}
	for i := range n {
		inv.APIUsage = append(inv.APIUsage, inventory.APIUsage{Group: "g", Version: "v", Kind: fmt.Sprintf("K%d", i), Count: 1})
		inv.DeprecatedCalls = append(inv.DeprecatedCalls, inventory.DeprecatedCall{Group: "g", Version: "v", Resource: fmt.Sprintf("r%d", i)})
	}
	return inv
}

// #236: folding was O(calls × usage), 13 to 53 s for 16,000 of each.
// Indexed, it is linear: the bound here is generous for a loaded or
// race-instrumented machine; the measured time is in the claims ledger
// (DC-05).
func TestFoldDeprecatedCallsIsLinear(t *testing.T) {
	inv := foldInventory(16000)
	k := kb.KB{Skew: kb.DefaultSkewPolicy()}
	start := time.Now()
	rep := Evaluate(inv, k, inventory.Version{Major: 1, Minor: 31}, testNow)
	if d := time.Since(start); d > time.Second {
		t.Errorf("Evaluate with 16,000 usage entries and caller rows took %s, want well under a second", d)
	}
	if n := len(callerFindings(rep.Findings)); n != 16000 {
		t.Errorf("%d caller findings, want 16000", n)
	}
}

func BenchmarkFoldDeprecatedCalls16k(b *testing.B) {
	inv := foldInventory(16000)
	k := kb.KB{Skew: kb.DefaultSkewPolicy()}
	target := inventory.Version{Major: 1, Minor: 31}
	b.ReportAllocs()
	for b.Loop() {
		Evaluate(inv, k, target, testNow)
	}
}

// The index keeps the scan's first-match semantics and plural rules.
func TestFoldIndexMatchesKindMatchesResource(t *testing.T) {
	for _, kind := range []string{"Ingress", "Endpoints", "NetworkPolicy", "Gateway", "Status", "CronJob", "Y", "Ay"} {
		lower, plural := resourceNames(kind)
		for _, res := range []string{lower, plural, lower + "s", lower + "es", strings.TrimSuffix(lower, "y") + "ies", "x"} {
			want := kindMatchesResource(kind, res)
			got := res == lower || res == plural
			if got != want {
				t.Errorf("%s/%s: index %v, kindMatchesResource %v", kind, res, got, want)
			}
		}
	}
}

// Two usage entries whose kinds share a REST resource name (Status and
// Statuse both pluralize to statuses): the row folds into the first
// entry's finding, whichever order they come in.
func TestFoldIndexFirstUsageEntryWins(t *testing.T) {
	k := testKB()
	for _, kind := range []string{"Status", "Statuse"} {
		k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{Group: "example.com", Version: "v1beta1", Kind: kind,
			Introduced: inventory.Version{Major: 1, Minor: 20}, Deprecated: vp(1, 24)})
	}
	for _, kinds := range [][2]string{{"Status", "Statuse"}, {"Statuse", "Status"}} {
		inv := inventory.Inventory{
			APIUsage: []inventory.APIUsage{
				{Group: "example.com", Version: "v1beta1", Kind: kinds[0], Count: 1, Namespaces: map[string]int{"a": 1}},
				{Group: "example.com", Version: "v1beta1", Kind: kinds[1], Count: 1, Namespaces: map[string]int{"b": 1}},
			},
			DeprecatedCalls: []inventory.DeprecatedCall{{Group: "example.com", Version: "v1beta1", Resource: "statuses"}},
		}
		rep := Evaluate(inv, k, inventory.Version{Major: 1, Minor: 30}, testNow)
		callers := map[string]int{}
		for _, f := range rep.Findings {
			callers[f.Key] = len(f.Callers)
		}
		first, second := "deprecated-api/example.com/v1beta1/"+kinds[0], "deprecated-api/example.com/v1beta1/"+kinds[1]
		if len(rep.Findings) != 2 || callers[first] != 1 || callers[second] != 0 {
			t.Errorf("usage order %v: callers per finding %v, want the row on %s only", kinds, callers, first)
		}
	}
}

// kbRemovals keeps RemovalOfCall's first-entry-wins rule when two KB
// kinds share a REST resource name.
func TestKBRemovalsFirstEntryWins(t *testing.T) {
	for _, order := range [][2]string{{"Status", "Statuse"}, {"Statuse", "Status"}} {
		k := kb.KB{APILifecycle: []kb.APILifecycleEntry{
			{Group: "example.com", Version: "v1beta1", Kind: order[0], Removed: vp(1, 26)},
			{Group: "example.com", Version: "v1beta1", Kind: order[1], Removed: vp(1, 29)},
		}}
		got, ok := kbRemovals(k)[resourceKey("example.com", "v1beta1", "statuses")]
		if !ok || got != *vp(1, 26) {
			t.Errorf("entries %v: removal %v (%v), want the first entry's 1.26", order, got, ok)
		}
	}
}

// #237: a remediation names only an API the target serves; otherwise it
// says none is known, and when the next one is served from.
func TestRemediationNamesOnlyServedReplacement(t *testing.T) {
	k := shippedKB(t)
	cases := []struct {
		group, version, kind string
		target               int
		want                 string
	}{
		{"storage.k8s.io", "v1alpha1", "VolumeAttributesClass", 33, "migrate to storage.k8s.io/v1beta1 VolumeAttributesClass"},
		{"storage.k8s.io", "v1alpha1", "VolumeAttributesClass", 34, "migrate to storage.k8s.io/v1 VolumeAttributesClass"},
		{"certificates.k8s.io", "v1beta1", "ClusterTrustBundle", 36, "no replacement Kubernetes 1.36 serves is known; certificates.k8s.io/v1 ClusterTrustBundle is served from 1.37; certificates.k8s.io/v1beta1 is the right version for Kubernetes 1.36 until then"},
	}
	for _, tc := range cases {
		inv := inventory.Inventory{APIUsage: []inventory.APIUsage{{Group: tc.group, Version: tc.version, Kind: tc.kind, Count: 1, Namespaces: map[string]int{"": 1}}}}
		target := inventory.Version{Major: 1, Minor: tc.target}
		fs := evalAPIUsage(inv, k, target, nil)
		if len(fs) != 1 || fs[0].Remediation != tc.want {
			t.Errorf("%s/%s %s @%s: findings %+v, want remediation %q", tc.group, tc.version, tc.kind, target, fs, tc.want)
		}
	}
}

// A Helm manifest's remediation names only replacements the target
// serves, as an API usage finding's does (ResolveReplacement), and says
// when none is known: with the release that serves the chain's next
// version when the KB knows it, as evalAPIUsage words it.
func TestHelmManifestRemediationNamesOnlyServedReplacement(t *testing.T) {
	v := func(m int) *inventory.Version { return &inventory.Version{Major: 1, Minor: m} }
	k := kb.KB{
		Version: "test-kb-1",
		APILifecycle: []kb.APILifecycleEntry{
			// Widget: v1alpha1 is gone at 1.30 and v1 is served from 1.31.
			{Group: "example.io", Version: "v1alpha1", Kind: "Widget", Introduced: *v(20), Deprecated: v(25), Removed: v(30),
				Replacement: &kb.GVK{Group: "example.io", Version: "v1", Kind: "Widget"}},
			{Group: "example.io", Version: "v1", Kind: "Widget", Introduced: *v(31)},
			// Gadget: v1alpha1's replacement v1beta1 is gone by 1.30 too,
			// and nothing replaces it.
			{Group: "example.io", Version: "v1alpha1", Kind: "Gadget", Introduced: *v(20), Deprecated: v(25), Removed: v(30),
				Replacement: &kb.GVK{Group: "example.io", Version: "v1beta1", Kind: "Gadget"}},
			{Group: "example.io", Version: "v1beta1", Kind: "Gadget", Introduced: *v(22), Deprecated: v(26), Removed: v(29)},
			// Gizmo: v1beta1 is replaced by v1, served at 1.30.
			{Group: "example.io", Version: "v1beta1", Kind: "Gizmo", Introduced: *v(20), Deprecated: v(25), Removed: v(30),
				Replacement: &kb.GVK{Group: "example.io", Version: "v1", Kind: "Gizmo"}},
			{Group: "example.io", Version: "v1", Kind: "Gizmo", Introduced: *v(25)},
		},
		Skew: kb.DefaultSkewPolicy(),
	}
	row := func(version, kind string) inventory.APIUsage {
		return inventory.APIUsage{Group: "example.io", Version: version, Kind: kind, Count: 1, Namespaces: map[string]int{"": 1},
			Objects: []inventory.ObjectRef{{Name: strings.ToLower(kind)}}}
	}
	rel := inventory.HelmRelease{Name: "r", Namespace: "ns", ChartName: "c", ChartVersion: "1.0.0", Status: "deployed", Revision: 1,
		ManifestAPIs: []inventory.APIUsage{row("v1alpha1", "Widget"), row("v1alpha1", "Gadget"), row("v1beta1", "Gizmo")}}
	fs := evalHelmReleases(inventory.Inventory{HelmReleases: []inventory.HelmRelease{rel}}, k, inventory.Version{Major: 1, Minor: 30}, nil)
	const want = "upgrade the release to a chart version that renders supported APIs (example.io/v1 Gizmo) before upgrading the cluster; " +
		"if the cluster already stopped serving them, rewrite the stored manifest with the helm-mapkubeapis plugin first; " +
		"no replacement Kubernetes 1.30 serves is known for example.io/v1alpha1 Gadget, " +
		"example.io/v1alpha1 Widget (example.io/v1 Widget is served from 1.31)"
	if len(fs) != 1 || fs[0].Remediation != want {
		t.Errorf("findings = %+v\nwant one with remediation %q", fs, want)
	}
	// The warning a release deprecated at the target gets says so too.
	fs = evalHelmReleases(inventory.Inventory{HelmReleases: []inventory.HelmRelease{rel}}, k, inventory.Version{Major: 1, Minor: 29}, nil)
	const warn = "upgrade the release to a chart version that renders supported APIs (example.io/v1 Gizmo); " +
		"no replacement Kubernetes 1.29 serves is known for example.io/v1alpha1 Gadget, " +
		"example.io/v1alpha1 Widget (example.io/v1 Widget is served from 1.31)"
	if len(fs) != 1 || fs[0].Severity != SevWarning || fs[0].Remediation != warn {
		t.Errorf("findings at 1.29 = %+v\nwant one warning with remediation %q", fs, warn)
	}
}

// #332: a beta or GA source is never told to move back to an alpha. When
// the GA replacement is not served yet, the finding says the version in use
// is the right one for the target and names the GA version with the minor
// that first serves it; and a 1.16 target does not report the kinds gen-kb
// used to clamp to 1.17 as "not served until 1.17".
func TestRemediationNeverMovesBackToLessMatureAPI(t *testing.T) {
	k := shippedKB(t)
	for _, tc := range []struct {
		group, version, kind string
		target               int
		want                 string
	}{
		{"batch", "v1beta1", "CronJob", 18, "no replacement Kubernetes 1.18 serves is known; batch/v1 CronJob is served from 1.21; batch/v1beta1 is the right version for Kubernetes 1.18 until then"},
		{"discovery.k8s.io", "v1beta1", "EndpointSlice", 18, "no replacement Kubernetes 1.18 serves is known; discovery.k8s.io/v1 EndpointSlice is served from 1.21; discovery.k8s.io/v1beta1 is the right version for Kubernetes 1.18 until then"},
		{"", "v1", "Endpoints", 18, ""},
	} {
		inv := inventory.Inventory{APIUsage: []inventory.APIUsage{{Group: tc.group, Version: tc.version, Kind: tc.kind, Count: 1, Namespaces: map[string]int{"a": 1}}}}
		target := inventory.Version{Major: 1, Minor: tc.target}
		for _, f := range evalAPIUsage(inv, k, target, nil) {
			if strings.Contains(f.Remediation, "alpha") {
				t.Errorf("%s/%s %s @%s: remediation %q names an alpha", tc.group, tc.version, tc.kind, target, f.Remediation)
			}
			if tc.want != "" && !strings.HasPrefix(f.Remediation, tc.want) {
				t.Errorf("%s/%s %s @%s: remediation %q, want %q", tc.group, tc.version, tc.kind, target, f.Remediation, tc.want)
			}
		}
	}
	for _, gvk := range [][3]string{{"batch", "v2alpha1", "CronJob"}, {"scheduling.k8s.io", "v1alpha1", "PriorityClass"}, {"settings.k8s.io", "v1alpha1", "PodPreset"}} {
		inv := inventory.Inventory{Source: inventory.SourceFiles, APIUsage: []inventory.APIUsage{{Group: gvk[0], Version: gvk[1], Kind: gvk[2], Count: 1, Namespaces: map[string]int{"a": 1}, Objects: []inventory.ObjectRef{{Name: "x", Line: 1}}}}}
		for _, f := range evalAPIUsage(inv, k, inventory.Version{Major: 1, Minor: 16}, nil) {
			if strings.Contains(f.Title, "not served until") {
				t.Errorf("%s/%s %s @1.16: %q", gvk[0], gvk[1], gvk[2], f.Title)
			}
		}
	}
}
