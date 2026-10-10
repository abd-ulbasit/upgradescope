package engine

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func helmTestKB() kb.KB {
	v := func(m int) *inventory.Version { return &inventory.Version{Major: 1, Minor: m} }
	return kb.KB{
		Version: "test-kb-1",
		APILifecycle: []kb.APILifecycleEntry{
			{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Introduced: *v(26), Deprecated: v(29), Removed: v(32),
				Replacement: &kb.GVK{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "FlowSchema"}},
			{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "FlowSchema", Introduced: *v(29)},
			{Group: "batch", Version: "v1beta1", Kind: "CronJob", Introduced: *v(8), Deprecated: v(21), Removed: v(25),
				Replacement: &kb.GVK{Group: "batch", Version: "v1", Kind: "CronJob"}},
		},
		Skew:        kb.DefaultSkewPolicy(),
		MaxKnownK8s: inventory.Version{Major: 1, Minor: 36},
	}
}

// flowSchemaRelease is a release whose stored manifest holds one
// flowcontrol v1beta3 FlowSchema (cluster-scoped, so no namespace).
func flowSchemaRelease() inventory.HelmRelease {
	return inventory.HelmRelease{
		Name: "apf", Namespace: "platform", ChartName: "apf", ChartVersion: "0.3.0", Status: "deployed", Revision: 2,
		ManifestAPIs: []inventory.APIUsage{{
			Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Count: 1,
			Namespaces: map[string]int{"": 1},
			Objects:    []inventory.ObjectRef{{Name: "batch-jobs", Line: 9, RenderedFrom: "apf/templates/flowschema.yaml"}},
		}},
	}
}

func findingKeys(fs []Finding) []string {
	var keys []string
	for _, f := range fs {
		keys = append(keys, f.Key)
	}
	return keys
}

// Helm refuses to install or upgrade a chart whose Chart.yaml kubeVersion
// excludes the cluster version (Masterminds semver; "-0" admits vendor
// pre-release builds such as v1.33.1-gke.100).
func TestEvalHelmChartKubeVersion(t *testing.T) {
	cases := []struct {
		constraint string
		target     int
		want       Severity // "" = no finding
	}{
		{">=1.21.0-0 <1.33.0-0", 33, SevBlocker},
		{">=1.21.0-0 <1.33.0-0", 32, ""},
		{">=1.21.0-0", 33, ""},
		{">=1.21.0-0 <1.30.0-0", 30, SevBlocker},
		{">= 1.19.0-0, < 1.31.0-0", 31, SevBlocker},
		{">=1.34.0-0", 33, SevBlocker}, // too new for the target, too
		{"^1.20.0-0", 33, ""},
		{"", 33, ""},
		{">=1.21.0-0 <<1.30", 33, SevInfo}, // unparseable: not assessed, never a blocker
	}
	for _, tc := range cases {
		t.Run(tc.constraint, func(t *testing.T) {
			rel := inventory.HelmRelease{Name: "web", Namespace: "shop", ChartName: "web", ChartVersion: "2.0.0", KubeVersion: tc.constraint, Status: "deployed", Revision: 3}
			inv := inventory.Inventory{
				HelmReleases: []inventory.HelmRelease{rel},
				Namespaces:   []inventory.NamespaceInfo{{Name: "shop", Team: "payments"}},
			}
			fs := evalHelmReleases(inv, helmTestKB(), inventory.Version{Major: 1, Minor: tc.target}, nil)
			if tc.want == "" {
				if len(fs) != 0 {
					t.Fatalf("findings = %+v, want none", fs)
				}
				return
			}
			if len(fs) != 1 {
				t.Fatalf("findings = %+v, want one %s", fs, tc.want)
			}
			f := fs[0]
			if f.Severity != tc.want || f.Category != CatChartIncompat || f.Key != "chart-incompat/helm-release/shop/web" ||
				!slices.Equal(f.Namespaces, []string{"shop"}) || !slices.Equal(f.Teams, []string{"payments"}) ||
				!slices.Contains(f.Citations, helmChartYAMLURL) {
				t.Errorf("finding = %+v", f)
			}
		})
	}
}

func TestEvalHelmChartKubeVersionText(t *testing.T) {
	inv := inventory.Inventory{HelmReleases: []inventory.HelmRelease{{
		Name: "web", Namespace: "shop", ChartName: "web", ChartVersion: "2.0.0", KubeVersion: ">=1.21.0-0 <1.33.0-0", Status: "deployed", Revision: 3,
	}}}
	fs := evalHelmReleases(inv, helmTestKB(), inventory.Version{Major: 1, Minor: 33}, nil)
	want := Finding{
		Category: CatChartIncompat, Severity: SevBlocker,
		Key:         "chart-incompat/helm-release/shop/web",
		Title:       `Helm release shop/web: chart web 2.0.0 requires Kubernetes ">=1.21.0-0 <1.33.0-0" (target 1.33)`,
		Detail:      `Chart web 2.0.0 (revision 3 of Helm release shop/web) declares kubeVersion ">=1.21.0-0 <1.33.0-0" in Chart.yaml, which Kubernetes 1.33 does not satisfy. Helm refuses to install or upgrade a chart whose kubeVersion excludes the cluster version, so once the cluster runs 1.33 every helm upgrade of this release fails until it moves to a chart version that supports 1.33.`,
		Namespaces:  []string{"shop"},
		Remediation: "upgrade the release to a chart version whose kubeVersion includes 1.33 before upgrading the cluster",
		Citations:   []string{helmChartYAMLURL},
	}
	if len(fs) != 1 || !reflect.DeepEqual(fs[0], want) {
		t.Errorf("findings = %#v\nwant %#v", fs, want)
	}
}

// #26: a release whose stored manifest uses an API the target (or already
// the cluster) no longer serves blocks: helm upgrade fails on it. The live
// collector cannot see these objects once the API is gone.
func TestEvalHelmManifestRemovedAPIBlocks(t *testing.T) {
	inv := inventory.Inventory{
		ServerVersion: "v1.32.3",
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapVersions: {Available: true}, inventory.CapAPIUsage: {Available: true}, inventory.CapHelm: {Available: true},
		},
		HelmReleases: []inventory.HelmRelease{flowSchemaRelease()},
	}
	rep := Evaluate(inv, helmTestKB(), inventory.Version{Major: 1, Minor: 32}, testNow)
	want := Finding{
		Category: CatRemovedAPI, Severity: SevBlocker,
		Key:         "removed-api/helm-release/platform/apf",
		Title:       "helm upgrade of release platform/apf will fail: its manifest uses flowcontrol.apiserver.k8s.io/v1beta3 FlowSchema",
		Detail:      "Revision 2 of Helm release platform/apf (chart apf 0.3.0) stores a manifest with 1 object at APIs Kubernetes 1.32 does not serve: flowcontrol.apiserver.k8s.io/v1beta3 FlowSchema (removed in 1.32; 1 object). Helm refuses to upgrade a release whose stored manifest uses APIs the cluster no longer serves.",
		Namespaces:  []string{"platform"},
		Remediation: "upgrade the release to a chart version that renders supported APIs (flowcontrol.apiserver.k8s.io/v1 FlowSchema) before upgrading the cluster; if the cluster already stopped serving them, rewrite the stored manifest with the helm-mapkubeapis plugin first",
		Citations:   []string{helmKubernetesAPIsURL, deprecationGuideURL},
		Objects:     []inventory.ObjectRef{{Name: "batch-jobs", Line: 9, RenderedFrom: "apf/templates/flowschema.yaml"}},
	}
	if len(rep.Findings) != 1 || !reflect.DeepEqual(rep.Findings[0], want) {
		t.Errorf("findings = %#v\nwant %#v", rep.Findings, want)
	}
	if rep.Ready || rep.Verdict != VerdictBlocked {
		t.Errorf("ready = %v, verdict %s; want false, blocked", rep.Ready, rep.Verdict)
	}
}

func TestEvalHelmManifestDeprecatedAPIWarns(t *testing.T) {
	inv := inventory.Inventory{HelmReleases: []inventory.HelmRelease{flowSchemaRelease()}}
	fs := evalHelmReleases(inv, helmTestKB(), inventory.Version{Major: 1, Minor: 30}, nil)
	want := Finding{
		Category: CatDeprecatedAPI, Severity: SevWarning,
		Key:         "deprecated-api/helm-release/platform/apf",
		Title:       "Helm release platform/apf manifest uses deprecated flowcontrol.apiserver.k8s.io/v1beta3 FlowSchema",
		Detail:      "Revision 2 of Helm release platform/apf (chart apf 0.3.0) stores a manifest with 1 object at deprecated APIs: flowcontrol.apiserver.k8s.io/v1beta3 FlowSchema (deprecated in 1.29, removed in 1.32; 1 object).",
		Namespaces:  []string{"platform"},
		Remediation: "upgrade the release to a chart version that renders supported APIs (flowcontrol.apiserver.k8s.io/v1 FlowSchema)",
		Citations:   []string{helmKubernetesAPIsURL, deprecationGuideURL},
		Objects:     []inventory.ObjectRef{{Name: "batch-jobs", Line: 9, RenderedFrom: "apf/templates/flowschema.yaml"}},
	}
	if len(fs) != 1 || !reflect.DeepEqual(fs[0], want) {
		t.Errorf("findings = %#v\nwant %#v", fs, want)
	}
}

// An object the live collector already flags (the API is still served and
// someone writes through it) is reported once, by the live finding; the
// release's finding keeps only the objects the live scan did not flag.
func TestEvalHelmManifestNotDoubleCounted(t *testing.T) {
	live := inventory.APIUsage{
		Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Count: 1,
		Namespaces: map[string]int{"": 1},
		Objects:    []inventory.ObjectRef{{Name: "batch-jobs", Manager: "helm"}},
	}
	target := inventory.Version{Major: 1, Minor: 32}

	inv := inventory.Inventory{APIUsage: []inventory.APIUsage{live}, HelmReleases: []inventory.HelmRelease{flowSchemaRelease()}}
	rep := Evaluate(inv, helmTestKB(), target, testNow)
	if got := findingKeys(rep.Findings); !slices.Equal(got, []string{"removed-api/flowcontrol.apiserver.k8s.io/v1beta3/FlowSchema"}) {
		t.Errorf("finding keys = %v, want only the live finding", got)
	}

	// A namespaced object the chart renders without metadata.namespace is
	// the live object in the release namespace.
	cron := inventory.HelmRelease{
		Name: "jobs", Namespace: "batch", ChartName: "jobs", ChartVersion: "1.0.0", Status: "deployed", Revision: 1,
		ManifestAPIs: []inventory.APIUsage{{
			Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 2,
			Namespaces: map[string]int{"": 2},
			Objects:    []inventory.ObjectRef{{Name: "nightly", Line: 4}, {Name: "weekly", Line: 20}},
		}},
	}
	inv = inventory.Inventory{
		APIUsage: []inventory.APIUsage{{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 1,
			Namespaces: map[string]int{"batch": 1}, Objects: []inventory.ObjectRef{{Namespace: "batch", Name: "nightly", Manager: "helm"}}}},
		HelmReleases: []inventory.HelmRelease{cron},
	}
	fs := evalHelmReleases(inv, helmTestKB(), inventory.Version{Major: 1, Minor: 25}, nil)
	if len(fs) != 1 || !reflect.DeepEqual(fs[0].Objects, []inventory.ObjectRef{{Name: "weekly", Line: 20}}) ||
		fs[0].Title != "helm upgrade of release batch/jobs will fail: its manifest uses batch/v1beta1 CronJob" {
		t.Errorf("findings = %+v, want one blocker naming only the weekly CronJob", fs)
	}
}

// One finding per release and severity, whatever the number of APIs.
func TestEvalHelmManifestOneFindingPerReleaseAndSeverity(t *testing.T) {
	rel := flowSchemaRelease()
	rel.ManifestAPIs = append(rel.ManifestAPIs, inventory.APIUsage{
		Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 2, Namespaces: map[string]int{"": 2},
		Objects: []inventory.ObjectRef{{Name: "a", Line: 30}, {Name: "b", Line: 50}},
	})
	fs := evalHelmReleases(inventory.Inventory{HelmReleases: []inventory.HelmRelease{rel}}, helmTestKB(), inventory.Version{Major: 1, Minor: 32}, nil)
	if len(fs) != 1 || fs[0].Severity != SevBlocker || len(fs[0].Objects) != 3 ||
		fs[0].Title != "helm upgrade of release platform/apf will fail: its manifest uses batch/v1beta1 CronJob, flowcontrol.apiserver.k8s.io/v1beta3 FlowSchema" {
		t.Errorf("findings = %+v, want one blocker for both APIs", fs)
	}
	fs = evalHelmReleases(inventory.Inventory{HelmReleases: []inventory.HelmRelease{rel}}, helmTestKB(), inventory.Version{Major: 1, Minor: 25}, nil)
	if got := findingKeys(fs); !slices.Equal(got, []string{"removed-api/helm-release/platform/apf", "deprecated-api/helm-release/platform/apf"}) {
		t.Errorf("finding keys = %v, want a blocker (CronJob) and a warning (FlowSchema)", got)
	}
}

// Inventories from agents that predate installed-state selection may list
// releases removed with helm uninstall --keep-history: nothing is running.
func TestEvalHelmSkipsUninstalledReleases(t *testing.T) {
	for _, status := range []string{"uninstalled", "uninstalling"} {
		rel := flowSchemaRelease()
		rel.Status = status
		rel.KubeVersion = "<1.20.0-0"
		if fs := evalHelmReleases(inventory.Inventory{HelmReleases: []inventory.HelmRelease{rel}}, helmTestKB(), inventory.Version{Major: 1, Minor: 32}, nil); len(fs) != 0 {
			t.Errorf("%s: findings = %+v, want none", status, fs)
		}
	}
}

// A stored manifest's removal past the KB horizon is a projection, not a
// shipped release, and the Helm findings say so as the live and manifest
// findings do (#266, API-02b): in the blocker (removal at or before the
// target) and in the warning (removal in the next minor).
func TestEvalHelmManifestRemovalPastHorizonIsProjected(t *testing.T) {
	k := helmTestKB() // horizon 1.36
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Group: "example.k8s.io", Version: "v1alpha1", Kind: "Widget", Introduced: inventory.Version{Major: 1, Minor: 35},
		Deprecated: &inventory.Version{Major: 1, Minor: 37}, Removed: &inventory.Version{Major: 1, Minor: 38},
		Replacement: &kb.GVK{Group: "example.k8s.io", Version: "v1", Kind: "Widget"},
	}, kb.APILifecycleEntry{Group: "example.k8s.io", Version: "v1", Kind: "Widget", Introduced: inventory.Version{Major: 1, Minor: 36}})
	rel := inventory.HelmRelease{
		Name: "w", Namespace: "apps", ChartName: "w", ChartVersion: "1.0.0", Status: "deployed", Revision: 1,
		ManifestAPIs: []inventory.APIUsage{{Group: "example.k8s.io", Version: "v1alpha1", Kind: "Widget", Count: 1,
			Namespaces: map[string]int{"apps": 1}, Objects: []inventory.ObjectRef{{Name: "x", Namespace: "apps", Line: 3}}}},
	}
	inv := inventory.Inventory{HelmReleases: []inventory.HelmRelease{rel}}
	for _, c := range []struct {
		target int
		sev    Severity
		key    string
	}{
		{38, SevBlocker, "removed-api/helm-release/apps/w"}, // the removal is at the target
		{39, SevBlocker, "removed-api/helm-release/apps/w"}, // and before it
		{37, SevWarning, "deprecated-api/helm-release/apps/w"},
	} {
		fs := evalHelmReleases(inv, k, inventory.Version{Major: 1, Minor: c.target}, nil)
		if len(fs) != 1 || fs[0].Severity != c.sev || fs[0].Key != c.key {
			t.Fatalf("target 1.%d: findings = %+v, want one %s %s", c.target, fs, c.sev, c.key)
		}
		f := fs[0]
		if !strings.Contains(f.Detail, "removed in 1.38 (projected)") || !strings.Contains(f.Detail, "The removal in 1.38 is projected") ||
			!strings.Contains(f.Detail, "the newest the knowledge base covers is 1.36") {
			t.Errorf("target 1.%d: detail = %q, want the removal marked projected with the projection sentence", c.target, f.Detail)
		}
		if c.sev == SevBlocker && !strings.Contains(f.Detail, "is projected not to serve") {
			t.Errorf("target 1.%d: detail = %q, want 'is projected not to serve', not a statement of fact", c.target, f.Detail)
		}
	}
	// Inside the horizon the removal is a fact: no marker (FlowSchema, removed 1.32).
	fs := evalHelmReleases(inventory.Inventory{HelmReleases: []inventory.HelmRelease{flowSchemaRelease()}}, k, inventory.Version{Major: 1, Minor: 32}, nil)
	if len(fs) != 1 || strings.Contains(fs[0].Detail, "projected") || strings.Contains(fs[0].Title, "projected") || !strings.Contains(fs[0].Detail, "does not serve") {
		t.Errorf("a removal inside the horizon: findings = %+v, want no projected marker", fs)
	}
}

// The horizon is the newest release the dataset covers: a stored manifest's
// removal AT it is a shipped release and stated as a fact, as evalAPIUsage
// does; only a removal strictly after it is "(projected)".
func TestEvalHelmManifestRemovalAtHorizonIsNotProjected(t *testing.T) {
	k := helmTestKB() // horizon 1.36
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Group: "example.k8s.io", Version: "v1beta1", Kind: "Widget", Introduced: inventory.Version{Major: 1, Minor: 34},
		Deprecated: &inventory.Version{Major: 1, Minor: 35}, Removed: &k.MaxKnownK8s,
		Replacement: &kb.GVK{Group: "example.k8s.io", Version: "v1", Kind: "Widget"},
	}, kb.APILifecycleEntry{Group: "example.k8s.io", Version: "v1", Kind: "Widget", Introduced: inventory.Version{Major: 1, Minor: 34}})
	rel := inventory.HelmRelease{
		Name: "w", Namespace: "apps", ChartName: "w", ChartVersion: "1.0.0", Status: "deployed", Revision: 1,
		ManifestAPIs: []inventory.APIUsage{{Group: "example.k8s.io", Version: "v1beta1", Kind: "Widget", Count: 1,
			Namespaces: map[string]int{"apps": 1}, Objects: []inventory.ObjectRef{{Name: "x", Namespace: "apps", Line: 3}}}},
	}
	inv := inventory.Inventory{HelmReleases: []inventory.HelmRelease{rel}}
	for _, c := range []struct {
		target int
		sev    Severity
		key    string
	}{
		{35, SevWarning, "deprecated-api/helm-release/apps/w"}, // the removal is in the next minor
		{36, SevBlocker, "removed-api/helm-release/apps/w"},    // at the target, and the horizon
		{37, SevBlocker, "removed-api/helm-release/apps/w"},    // one release past the horizon, the removal is still a fact
	} {
		fs := evalHelmReleases(inv, k, inventory.Version{Major: 1, Minor: c.target}, nil)
		if len(fs) != 1 || fs[0].Severity != c.sev || fs[0].Key != c.key {
			t.Fatalf("target 1.%d: findings = %+v, want one %s %s", c.target, fs, c.sev, c.key)
		}
		f := fs[0]
		if !strings.Contains(f.Detail, "removed in 1.36") || strings.Contains(f.Detail, "projected") || strings.Contains(f.Title, "projected") {
			t.Errorf("target 1.%d: title %q detail %q, want the removal at the horizon stated as a fact", c.target, f.Title, f.Detail)
		}
		if c.sev == SevBlocker && !strings.Contains(f.Detail, "does not serve") {
			t.Errorf("target 1.%d: detail = %q, want 'does not serve'", c.target, f.Detail)
		}
	}
}
