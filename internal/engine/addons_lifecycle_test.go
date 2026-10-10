package engine

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// lifecycleKB carries endoflife.date's Istio cycles as synced on 2026-10-02
// and a hand-curated entry with compat rows but no lifecycle data.
func lifecycleKB() kb.KB {
	eol := func(d string) *registry.CycleEOL { return &registry.CycleEOL{Date: d} }
	cite := []string{"https://endoflife.date/istio"}
	matrix := []string{"https://github.com/kubernetes-sigs/external-dns#kubernetes-version-compatibility"}
	return kb.KB{
		Version: "test-kb-1",
		AddOns: []registry.AddOn{
			{
				SchemaVersion: 2, ID: "istio", DisplayName: "Istio",
				Matchers: registry.Matchers{Images: []string{"istio/proxyv2"}},
				Support: registry.Support{Status: "supported",
					Citations: []string{"https://endoflife.date/istio", "https://istio.io/latest/docs/releases/supported-releases/"}},
				Cycles: []registry.Cycle{
					{Cycle: "1.31", EOL: eol("2027-02-28"), K8sMin: "1.32", K8sMax: "1.36", Citations: cite},
					{Cycle: "1.30", EOL: eol("2026-12-31"), K8sMin: "1.32", K8sMax: "1.36", Citations: cite},
					{Cycle: "1.29", EOL: eol("2026-10-31"), K8sMin: "1.31", K8sMax: "1.35", Citations: cite},
					{Cycle: "1.28", EOL: eol("2026-07-01"), K8sMin: "1.30", K8sMax: "1.34", Citations: cite},
					{Cycle: "1.27", EOL: eol("2026-04-07"), K8sMin: "1.29", K8sMax: "1.33", Citations: cite},
					{Cycle: "1.5", EOL: &registry.CycleEOL{Ended: true}, Citations: cite},
				},
				EndoflifeProduct: "istio",
			},
			{
				SchemaVersion: 2, ID: "external-dns", DisplayName: "ExternalDNS",
				Matchers: registry.Matchers{Images: []string{"external-dns/external-dns"}},
				Support:  registry.Support{Status: "supported", Citations: []string{"https://github.com/kubernetes-sigs/external-dns/releases"}},
				Compat: []registry.Compat{
					{Range: "<0.10.0", K8sMax: "1.21", Citations: matrix},
					{Range: ">=0.10.0 <0.18.0", K8sMin: "1.19", K8sMax: "1.32", Citations: matrix},
				},
			},
		},
		Skew:        kb.DefaultSkewPolicy(),
		MaxKnownK8s: inventory.Version{Major: 1, Minor: 37},
	}
}

func addOnAt(id, version string) inventory.Inventory {
	return inventory.Inventory{AddOns: []inventory.AddOnInstance{{ID: id, Version: version, Namespaces: []string{"istio-system"}, Source: "image"}}}
}

func day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// summarize renders findings as "severity category key title" lines.
func summarize(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s %s %s %s", f.Severity, f.Category, f.Key, f.Title))
	}
	return out
}

// Regression for the v0.1 time bomb: the newest cycle's date was applied to
// every installed version, so Istio 1.31 became a blocker on 2026-11-30
// while Istio 1.27, EOL since April, got only a warning.
func TestEvalAddOnsJudgesTheInstalledCycle(t *testing.T) {
	v135 := inventory.Version{Major: 1, Minor: 35}

	// 1.31 at 2026-12-01: supported until 2027-02-28, so no blocker; the
	// 90-day window is open, and the warning is about 1.31's own date.
	fs := evalAddOns(addOnAt("istio", "1.31.1"), lifecycleKB(), v135, day("2026-12-01"))
	want := []string{"warning eol-approaching eol-approaching/istio/1.31 Istio 1.31 reaches end-of-life on 2027-02-28"}
	if got := summarize(fs); !reflect.DeepEqual(got, want) {
		t.Fatalf("Istio 1.31 at 2026-12-01:\n got  %q\nwant %q", got, want)
	}
	if fs := evalAddOns(addOnAt("istio", "1.31.1"), lifecycleKB(), v135, day("2026-10-02")); len(fs) != 0 {
		t.Fatalf("Istio 1.31 on 2026-10-02 must yield nothing, got %+v", fs)
	}

	fs = evalAddOns(addOnAt("istio", "1.27.3"), lifecycleKB(), inventory.Version{Major: 1, Minor: 33}, day("2026-10-02"))
	wantF := []Finding{{
		Category:    CatEOLAddon,
		Severity:    SevBlocker,
		Key:         "eol-addon/istio/1.27",
		Title:       "Istio 1.27 is end-of-life since 2026-04-07",
		Detail:      "Detected Istio version 1.27.3 via image in namespace(s): istio-system. Upstream support for the 1.27 release line ended on 2026-04-07.",
		Namespaces:  []string{"istio-system"},
		Remediation: "Upgrade Istio to a supported release line (newest: 1.31).",
		Citations:   []string{"https://endoflife.date/istio", "https://istio.io/latest/docs/releases/supported-releases/"},
	}}
	if !reflect.DeepEqual(fs, wantF) {
		t.Fatalf("Istio 1.27:\n got %+v\nwant %+v", fs, wantF)
	}
}

func TestEvalAddOnsCycleLifecycle(t *testing.T) {
	cases := []struct {
		name, version string
		target        inventory.Version
		want          []string
	}{
		{"cycle ends within 90 days", "1.29.8", inventory.Version{Major: 1, Minor: 34},
			[]string{"warning eol-approaching eol-approaching/istio/1.29 Istio 1.29 reaches end-of-life on 2026-10-31"}},
		{"cycle ended without a published date", "1.5.10", inventory.Version{Major: 1, Minor: 34},
			[]string{"blocker eol-addon eol-addon/istio/1.5 Istio 1.5 is end-of-life"}},
		{"target above the cycle's k8s_max", "1.31.1", inventory.Version{Major: 1, Minor: 37},
			[]string{"blocker chart-incompat chart-incompat/istio/1.31 Istio 1.31.1 supports Kubernetes up to 1.36 (target 1.37)"}},
		{"target below the cycle's k8s_min", "1.31.1", inventory.Version{Major: 1, Minor: 31},
			[]string{"blocker chart-incompat chart-incompat/istio/1.31 Istio 1.31.1 requires Kubernetes 1.32 or newer (target 1.31)"}},
		{"pre-release maps to its cycle", "1.31.0-rc.0", inventory.Version{Major: 1, Minor: 35}, nil},
		// #165: older than the oldest tracked line (1.5, ended) is past end
		// of life; between tracked lines or newer than all of them, no data.
		{"version older than the oldest tracked line", "1.4.2", inventory.Version{Major: 1, Minor: 34},
			[]string{"blocker eol-addon eol-addon/istio/below-1.5 Istio 1.4.2 is end-of-life (older than the 1.5 release line)"}},
		{"version between tracked lines", "1.10.0", inventory.Version{Major: 1, Minor: 34},
			[]string{"info addon-no-data addon-no-data/istio no lifecycle data for Istio 1.10.0"}},
		{"version newer than the newest tracked line", "1.32.0", inventory.Version{Major: 1, Minor: 34},
			[]string{"info addon-no-data addon-no-data/istio no lifecycle data for Istio 1.32.0"}},
		{"major-only version is not judged against a minor line", "1", inventory.Version{Major: 1, Minor: 34},
			[]string{"info addon-no-data addon-no-data/istio no lifecycle data for Istio 1"}},
		{"unknown version", "", inventory.Version{Major: 1, Minor: 34},
			[]string{"info addon-no-data addon-no-data/istio no lifecycle data for Istio (version unknown)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize(evalAddOns(addOnAt("istio", tc.version), lifecycleKB(), tc.target, day("2026-10-02")))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestEvalAddOnsCycleCompatDetail(t *testing.T) {
	fs := evalAddOns(addOnAt("istio", "1.31.1"), lifecycleKB(), inventory.Version{Major: 1, Minor: 37}, day("2026-10-02"))
	if len(fs) != 1 {
		t.Fatalf("want one compat finding, got %+v", fs)
	}
	if want := "Installed version 1.31.1 is in the 1.31 release line, which supports Kubernetes 1.32 through 1.36."; fs[0].Detail != want {
		t.Errorf("detail = %q, want %q", fs[0].Detail, want)
	}
	if want := []string{"https://endoflife.date/istio"}; !reflect.DeepEqual(fs[0].Citations, want) {
		t.Errorf("citations = %v, want %v", fs[0].Citations, want)
	}
}

func TestEvalAddOnsNoDataDetail(t *testing.T) {
	// A collector that takes a pod's version label for an image or chart
	// without a version (inventory.LabelVersionCollectorSchema).
	current := func(inv inventory.Inventory) inventory.Inventory {
		inv.CollectorSchema = inventory.LabelVersionCollectorSchema
		return inv
	}
	fs := evalAddOns(current(addOnAt("istio", "")), lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
	want := "Detected Istio version (unknown) via image in namespace(s): istio-system. No version was read from the image: either its tag names no version (a digest, :latest) and no pod running it whose labels name this add-on gives a usable app.kubernetes.io/version, or it is a component image whose release line the registry does not map yet. Its end of life and Kubernetes compatibility were not assessed."
	if len(fs) != 1 || fs[0].Detail != want || fs[0].Severity != SevInfo {
		t.Fatalf("got %+v, want one info with detail %q", fs, want)
	}
	// What is missing follows what the add-on was found by (#301): the
	// detail never claims a source the detector did not consult.
	for _, tc := range []struct{ source, chart, want string }{
		{"labels", "", "The pod labels name it, but no app.kubernetes.io/version label gives a version that applies to it."},
		{"chart", "1.14.5", "The Helm release records no appVersion, and no pod image tag or app.kubernetes.io/version label gives a version."},
		{"gitops", "", "The GitOps chart reference gives no app version, and no running pod's image tag or app.kubernetes.io/version label does."},
		{"ingressclass", "", "An IngressClass names it but carries no version."},
		// A hand-written or third-party inventory may carry a source the
		// engine does not know, or none: no node-runtime sentence for it.
		{"", "", "No version was recorded for this add-on."},
		{"somethingelse", "", "No version was recorded for this add-on."},
	} {
		inv := current(inventory.Inventory{AddOns: []inventory.AddOnInstance{{ID: "istio", Namespaces: []string{"istio-system"}, Source: tc.source, ChartVersion: tc.chart}}})
		fs = evalAddOns(inv, lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
		if len(fs) != 1 || !strings.Contains(fs[0].Detail, " "+tc.want+" Its end of life and Kubernetes compatibility were not assessed.") ||
			strings.Contains(fs[0].Detail, "No version could be read") {
			t.Errorf("source %s: got %+v, want a detail with %q", tc.source, fs, tc.want)
		}
	}
	fs = evalAddOns(addOnAt("istio", "1.10.0"), lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
	want = "Detected Istio version 1.10.0 via image in namespace(s): istio-system. The registry has no release-line data for this version, so its end of life was not assessed."
	if len(fs) != 1 || fs[0].Detail != want {
		t.Fatalf("got %+v, want detail %q", fs, want)
	}
}

// An inventory from an agent that predates the pod-label rule (#301:
// v0.1.x and v0.2.0's release candidates stamp no CollectorSchema) never
// took a pod's app.kubernetes.io/version label for an image or chart
// without a version. A server that evaluates it after an upgrade must not
// say the label was unreadable or gave nothing (AO-08): it names the tag or
// component reason and says the agent did not read the label. The stamp
// decides: any collector schema from the rule's generation on gets the
// sentence that the label was consulted.
func TestEvalAddOnsNoDataDetailSaysWhenTheAgentDidNotReadPodLabels(t *testing.T) {
	const (
		image = "No version was read from the image: either its tag names no version (a digest, :latest) or it is a component image whose release line the registry does not map yet. The collecting agent predates reading a pod's app.kubernetes.io/version label for an image without a tag, so such a label may carry the version; upgrade the agent."
		chart = "No app version was read from the Helm release or from a pod image tag: either the release records none, or the collecting agent did not collect it (a v0.1.x agent reported only the chart version). The collecting agent predates reading a pod's app.kubernetes.io/version label for an image without a tag, so such a label may carry the version; upgrade the agent."
		gitop = "The GitOps chart reference gives no app version, and no running pod's image tag does. The collecting agent predates reading a pod's app.kubernetes.io/version label for an image without a tag, so such a label may carry the version; upgrade the agent."
	)
	for _, tc := range []struct{ source, chart, want string }{
		{"image", "", image},
		{"chart", "1.14.5", chart},
		{"gitops", "", gitop},
	} {
		for _, schema := range []int{0, inventory.LabelVersionCollectorSchema} {
			inv := inventory.Inventory{CollectorSchema: schema, AddOns: []inventory.AddOnInstance{{ID: "istio", Namespaces: []string{"istio-system"}, Source: tc.source, ChartVersion: tc.chart}}}
			fs := evalAddOns(inv, lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
			if len(fs) != 1 || fs[0].Severity != SevInfo {
				t.Fatalf("%s, schema %d: got %+v, want one info", tc.source, schema, fs)
			}
			d := fs[0].Detail
			if schema == 0 {
				if !strings.Contains(d, " "+tc.want+" Its end of life and Kubernetes compatibility were not assessed.") {
					t.Errorf("%s, schema 0: detail %q lacks %q", tc.source, d, tc.want)
				}
				// Nothing may say a label was consulted and gave no version.
				for _, said := range []string{"gives a usable app.kubernetes.io/version", "image tag or app.kubernetes.io/version label", "records no appVersion", "no pod image tag gives"} {
					if strings.Contains(d, said) {
						t.Errorf("%s, schema 0: detail %q says the label was consulted (%q)", tc.source, d, said)
					}
				}
			} else if strings.Contains(d, "predates reading") || strings.Contains(d, "upgrade the agent") {
				t.Errorf("%s, schema %d: detail %q blames the agent, which read the label", tc.source, schema, d)
			}
		}
	}
	// A schema above the rule's generation reads the label too.
	inv := inventory.Inventory{CollectorSchema: inventory.LabelVersionCollectorSchema + 1, AddOns: []inventory.AddOnInstance{{ID: "istio", Namespaces: []string{"istio-system"}, Source: "image"}}}
	if fs := evalAddOns(inv, lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02")); len(fs) != 1 || strings.Contains(fs[0].Detail, "predates reading") {
		t.Errorf("later schema: got %+v, want the sentence that labels were read", fs)
	}
	// Node runtimes and labels-found installs read the same on either.
	inv = inventory.Inventory{AddOns: []inventory.AddOnInstance{{ID: "istio", Namespaces: []string{"istio-system"}, Source: "labels"}}}
	if fs := evalAddOns(inv, lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02")); len(fs) != 1 || strings.Contains(fs[0].Detail, "predates reading") {
		t.Errorf("labels, schema 0: got %+v, want no blame on the agent", fs)
	}
}

// #165: a version older than the oldest tracked release line was an
// addon-no-data info, so Istio 1.4 read READY 100/100. It is end of life,
// citing the oldest line and the product's lifecycle pages.
func TestEvalAddOnsPredatesTrackedLinesDetail(t *testing.T) {
	inv := installs("istio", "image", "mesh-a=1.4.2", "mesh-b=1.3.0")
	fs := evalAddOns(inv, lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
	want := []Finding{{
		Category: CatEOLAddon,
		Severity: SevBlocker,
		Key:      "eol-addon/istio/below-1.5",
		Title:    "Istio 1.3.0 is end-of-life (older than the 1.5 release line)",
		Detail: "Detected Istio in namespace(s): mesh-a (1.4.2 via image), mesh-b (1.3.0 via image). " +
			"Versions older than the 1.5 release line, the oldest one upstream still documents, are past end of life: support for 1.5 has ended.",
		Teams:       []string{"ateam", "bteam"},
		Namespaces:  []string{"mesh-a", "mesh-b"},
		Remediation: "Upgrade Istio to a supported release line (newest: 1.31).",
		Citations:   []string{"https://endoflife.date/istio", "https://istio.io/latest/docs/releases/supported-releases/"},
	}}
	if !reflect.DeepEqual(fs, want) {
		t.Fatalf("got  %+v\nwant %+v", fs, want)
	}

	// A dated oldest line names its end.
	k := lifecycleKB()
	k.AddOns[0].Cycles = k.AddOns[0].Cycles[:len(k.AddOns[0].Cycles)-1] // drop 1.5: oldest is 1.27
	fs = evalAddOns(addOnAt("istio", "1.26.4"), k, inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
	if len(fs) != 1 || fs[0].Key != "eol-addon/istio/below-1.27" ||
		!strings.HasSuffix(fs[0].Detail, "support for 1.27 ended on 2026-04-07.") {
		t.Fatalf("got %+v, want one eol-addon/istio/below-1.27 naming 1.27's end", fs)
	}
}

// Below the oldest tracked line counts as ended only when that line has
// itself ended; while it is supported, the older version is no data.
func TestEvalAddOnsPredatesSupportedOldestLine(t *testing.T) {
	k := lifecycleKB()
	k.AddOns[0].Cycles = []registry.Cycle{{Cycle: "1.31", EOL: &registry.CycleEOL{Date: "2027-02-28"}, Citations: []string{"https://endoflife.date/istio"}}}
	got := summarize(evalAddOns(addOnAt("istio", "1.30.2"), k, inventory.Version{Major: 1, Minor: 34}, day("2026-10-02")))
	if want := []string{"info addon-no-data addon-no-data/istio no lifecycle data for Istio 1.30.2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// The red-team installs (#165) against the embedded registry: each is
// older than its product's oldest tracked line and must block, never read
// as no data; versions between or above the tracked lines, and products
// without lines, stay no data.
func TestEvalAddOnsPredatesTrackedLinesRealKB(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons, Skew: kb.DefaultSkewPolicy(), MaxKnownK8s: inventory.Version{Major: 1, Minor: 99}}
	now := day("2026-10-03")
	target := inventory.Version{Major: 1, Minor: 34}
	for _, tc := range []struct{ id, version string }{
		{"cert-manager", "1.5.0"},
		{"istio", "1.5.2"},
		{"calico", "3.24.5"},
		{"cilium", "1.12.0"},
		{"kyverno", "1.7.5"},
		{"argo-cd", "0.12.0"},
		{"flux", "1.24.0"},
		{"flux", "1.13.3"},
	} {
		var keys []string
		for _, f := range evalAddOns(addOnAt(tc.id, tc.version), k, target, now) {
			if f.Category == CatAddOnNoData {
				t.Errorf("%s %s: %s", tc.id, tc.version, f.Title)
			}
			if f.Category == CatEOLAddon && f.Severity == SevBlocker {
				keys = append(keys, f.Key)
				if !slices.Contains(f.Citations, addOnByID(t, addons, tc.id).Support.Citations[0]) {
					t.Errorf("%s %s: citations %v miss the product's lifecycle page", tc.id, tc.version, f.Citations)
				}
			}
		}
		if len(keys) != 1 || !strings.HasPrefix(keys[0], "eol-addon/"+tc.id+"/below-") {
			t.Errorf("%s %s: eol-addon blockers %q, want one eol-addon/%s/below-<oldest line>", tc.id, tc.version, keys, tc.id)
		}
	}
	for _, tc := range []struct{ id, version string }{
		{"flux", "1.30.0"},     // between 1.25 and 2.0
		{"argo-cd", "1.9.0"},   // between 1.8 and 2.0
		{"istio", "99.0.0"},    // newer than every line: the registry is stale
		{"velero", "0.1.0"},    // no lines
		{"coredns", "0.0.1"},   // no lines
		{"external-dns", "0"},  // no lines
		{"cert-manager", "1"},  // fewer components than any line
		{"calico", "latest"},   // no version
		{"cilium", ""},         // no version
		{"kyverno", "1.10.0"},  // a tracked line: judged by its own date
		{"cert-manager", "v0"}, // unparseable
	} {
		for _, f := range evalAddOns(addOnAt(tc.id, tc.version), k, target, now) {
			if strings.Contains(f.Key, "/below-") {
				t.Errorf("%s %s: %s %s", tc.id, tc.version, f.Key, f.Title)
			}
		}
	}
}

func addOnByID(t *testing.T, addons []registry.AddOn, id string) registry.AddOn {
	t.Helper()
	for _, a := range addons {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no registry entry %q", id)
	return registry.AddOn{}
}

// A Helm install is judged by its app version; the chart version is shown
// as evidence.
func TestEvalAddOnsHelmEvidence(t *testing.T) {
	inv := inventory.Inventory{AddOns: []inventory.AddOnInstance{{
		ID: "external-dns", Version: "0.14.2", ChartVersion: "1.14.5", Namespaces: []string{"dns"}, Source: "chart",
	}}}
	fs := evalAddOns(inv, lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
	if len(fs) == 0 || fs[0].Category != CatChartIncompat {
		t.Fatalf("want chart-incompat first, got %+v", fs)
	}
	if want := "Detected ExternalDNS version 0.14.2 via chart 1.14.5 in namespace(s): dns."; !strings.HasPrefix(fs[1].Detail, want) {
		t.Errorf("detail = %q, want prefix %q", fs[1].Detail, want)
	}
}

// Compat rows without lifecycle data: the row still fires, and the missing
// lifecycle data is reported as info, never as a blocker.
func TestEvalAddOnsCompatRowsWithoutCycles(t *testing.T) {
	cases := []struct {
		version string
		target  inventory.Version
		want    []string
	}{
		{"0.9.0", inventory.Version{Major: 1, Minor: 22}, []string{
			"blocker chart-incompat chart-incompat/external-dns ExternalDNS 0.9.0 supports Kubernetes up to 1.21 (target 1.22)",
			"info addon-no-data addon-no-data/external-dns no lifecycle data for ExternalDNS 0.9.0",
		}},
		{"0.14.2", inventory.Version{Major: 1, Minor: 34}, []string{
			"blocker chart-incompat chart-incompat/external-dns ExternalDNS 0.14.2 supports Kubernetes up to 1.32 (target 1.34)",
			"info addon-no-data addon-no-data/external-dns no lifecycle data for ExternalDNS 0.14.2",
		}},
		{"0.14.2", inventory.Version{Major: 1, Minor: 18}, []string{
			"blocker chart-incompat chart-incompat/external-dns ExternalDNS 0.14.2 requires Kubernetes 1.19 or newer (target 1.18)",
			"info addon-no-data addon-no-data/external-dns no lifecycle data for ExternalDNS 0.14.2",
		}},
		{"0.18.0", inventory.Version{Major: 1, Minor: 34}, []string{
			"info addon-no-data addon-no-data/external-dns no lifecycle data for ExternalDNS 0.18.0",
		}},
	}
	for _, tc := range cases {
		got := summarize(evalAddOns(addOnAt("external-dns", tc.version), lifecycleKB(), tc.target, day("2026-10-02")))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s at %s:\n got  %q\nwant %q", tc.version, tc.target, got, tc.want)
		}
	}
	fs := evalAddOns(addOnAt("external-dns", "0.9.0"), lifecycleKB(), inventory.Version{Major: 1, Minor: 22}, day("2026-10-02"))
	if want := `Installed version 0.9.0 matches compatibility range "<0.10.0", which supports Kubernetes up to 1.21.`; fs[0].Detail != want {
		t.Errorf("detail = %q, want %q", fs[0].Detail, want)
	}
}

// Every synced registry entry, evaluated a year from now at its newest
// cycle: no finding may come from a product-wide date (the v0.1 time bomb).
// An end-of-life finding is allowed only if the newest line itself ends
// within the year, and then it must name that line.
func TestEvalAddOnsSyncedEntriesHaveNoProductWideEOL(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().AddDate(0, 0, 365)
	k := kb.KB{AddOns: addons, Skew: kb.DefaultSkewPolicy(), MaxKnownK8s: inventory.Version{Major: 1, Minor: 99}}
	synced := 0
	for _, a := range addons {
		if a.EndoflifeProduct == "" {
			continue
		}
		synced++
		newest := a.Cycles[0]
		target := inventory.Version{Major: 1, Minor: 34}
		if newest.K8sMax != "" {
			target, _ = inventory.ParseVersion(newest.K8sMax)
		}
		for _, f := range evalAddOns(addOnAt(a.ID, newest.Cycle+".0"), k, target, now) {
			switch f.Category {
			case CatEOLAddon, CatEOLApproaching:
				if f.Key != string(f.Category)+"/"+a.ID+"/"+newest.Cycle {
					t.Errorf("%s %s: product-wide finding %+v", a.ID, newest.Cycle, f)
				}
			case CatAddOnNoData:
				t.Errorf("%s %s: newest cycle not mapped: %+v", a.ID, newest.Cycle, f)
			}
		}
	}
	if synced == 0 {
		t.Fatal("no synced entries in the embedded registry")
	}
}

// The embedded ExternalDNS matrix rows, both ends: ≤0.9.x stops at 1.21,
// 0.10–0.17 at 1.32, 0.18+ is open-ended.
func TestEmbeddedExternalDNSCompatRows(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons}
	cases := []struct {
		version string
		target  inventory.Version
		want    string // chart-incompat title, "" for none
	}{
		{"0.9.0", inventory.Version{Major: 1, Minor: 21}, ""},
		{"0.9.0", inventory.Version{Major: 1, Minor: 22}, "ExternalDNS 0.9.0 supports Kubernetes up to 1.21 (target 1.22)"},
		{"0.17.0", inventory.Version{Major: 1, Minor: 33}, "ExternalDNS 0.17.0 supports Kubernetes up to 1.32 (target 1.33)"},
		{"0.18.0", inventory.Version{Major: 1, Minor: 36}, ""},
	}
	for _, tc := range cases {
		got := ""
		for _, f := range evalAddOns(addOnAt("external-dns", tc.version), k, tc.target, day("2026-10-02")) {
			if f.Category == CatChartIncompat {
				got = f.Title
			}
		}
		if got != tc.want {
			t.Errorf("%s at %s: chart-incompat %q, want %q", tc.version, tc.target, got, tc.want)
		}
	}
}

// installs builds one add-on instance per "namespace=version" pair, each
// namespace owned by the team of the same name with "team" appended.
func installs(id, source string, pairs ...string) inventory.Inventory {
	var inv inventory.Inventory
	for _, p := range pairs {
		ns, ver, _ := strings.Cut(p, "=")
		inv.AddOns = append(inv.AddOns, inventory.AddOnInstance{ID: id, Version: ver, Namespaces: []string{ns}, Source: source})
		inv.Namespaces = append(inv.Namespaces, inventory.NamespaceInfo{Name: ns, Team: strings.TrimPrefix(ns, "mesh-") + "team"})
	}
	return inv
}

// whereSummary renders findings as "key ns=[...] teams=[...]" lines.
func whereSummary(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s %s ns=%v teams=%v", f.Severity, f.Key, f.Namespaces, f.Teams))
	}
	return out
}

// A mesh that mixes release lines is judged per line: the 1.28 blocker
// names only the namespace running 1.28, the 1.30 warning survives, and
// the team on 1.31 is not blamed for anyone else's install.
func TestEvalAddOnsGroupsByReleaseLine(t *testing.T) {
	inv := installs("istio", "image", "mesh-new=1.31.1", "mesh-mid=1.30.5", "mesh-old=1.28.10")
	target, now := inventory.Version{Major: 1, Minor: 36}, day("2026-10-02")
	fs := evalAddOns(inv, lifecycleKB(), target, now)
	sortFindings(fs)
	want := []string{
		"blocker chart-incompat/istio/1.28 ns=[mesh-old] teams=[oldteam]",
		"blocker eol-addon/istio/1.28 ns=[mesh-old] teams=[oldteam]",
		"warning eol-approaching/istio/1.30 ns=[mesh-mid] teams=[midteam]",
	}
	if got := whereSummary(fs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if want := "Detected Istio version 1.28.10 via image in namespace(s): mesh-old. Upstream support for the 1.28 release line ended on 2026-07-01."; fs[1].Detail != want {
		t.Errorf("detail = %q, want %q", fs[1].Detail, want)
	}
	// TeamScores scores the teams findings name: newteam has none, so it
	// is not scored down (the merge scored it 50).
	scores := TeamScores(Report{Findings: fs})
	if s, ok := scores["newteam"]; ok {
		t.Errorf("newteam = %+v, want no findings", s)
	}
	if s := scores["oldteam"]; s.Score != 50 || s.Ready {
		t.Errorf("oldteam = %+v, want 50, not ready", s)
	}
}

// The same mesh against the embedded registry (the #129 reproduction):
// istio 1.28 is past end of life and supports at most Kubernetes 1.34,
// 1.30 ends on 2026-12-31 (90 days out), and 1.31 is fine at 1.36.
func TestEvalAddOnsGroupsByReleaseLineRealKB(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	inv := installs("istio", "image", "mesh-new=1.31.1", "mesh-mid=1.30.5", "mesh-old=1.28.10")
	fs := evalAddOns(inv, k, inventory.Version{Major: 1, Minor: 36}, day("2026-10-02"))
	sortFindings(fs)
	want := []string{
		"blocker chart-incompat/istio/1.28 ns=[mesh-old] teams=[oldteam]",
		"blocker eol-addon/istio/1.28 ns=[mesh-old] teams=[oldteam]",
		"warning eol-approaching/istio/1.30 ns=[mesh-mid] teams=[midteam]",
	}
	if got := whereSummary(fs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// Two installs on one release line make one finding naming both, judged at
// the older one, and the detail names each namespace's version.
func TestEvalAddOnsReleaseLineNamesEachVersion(t *testing.T) {
	inv := installs("istio", "image", "mesh-a=1.29.2", "mesh-b=1.29.8")
	fs := evalAddOns(inv, lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
	want := []Finding{{
		Category: CatEOLApproaching, Severity: SevWarning, Key: "eol-approaching/istio/1.29",
		Title:       "Istio 1.29 reaches end-of-life on 2026-10-31",
		Detail:      "Detected Istio in namespace(s): mesh-a (1.29.2 via image), mesh-b (1.29.8 via image). Upstream support for the 1.29 release line ends on 2026-10-31.",
		Teams:       []string{"ateam", "bteam"},
		Namespaces:  []string{"mesh-a", "mesh-b"},
		Remediation: "Upgrade Istio to a supported release line (newest: 1.31).",
		Citations:   []string{"https://endoflife.date/istio", "https://istio.io/latest/docs/releases/supported-releases/"},
	}}
	if !reflect.DeepEqual(fs, want) {
		t.Fatalf("got  %+v\nwant %+v", fs, want)
	}
}

// A mesh with sidecars in many namespaces stays one bounded finding: the
// detail lists the first addOnLocatedLimit installs and counts the rest,
// whether their versions match, differ, or are per node; Namespaces and
// Teams still name every install.
func TestEvalAddOnsDetailBounded(t *testing.T) {
	var mixed, same []string
	for i := 1; i <= 25; i++ {
		mixed = append(mixed, fmt.Sprintf("mesh-%02d=1.29.%d", i, i))
		same = append(same, fmt.Sprintf("mesh-%02d=1.29.2", i))
	}
	target, now := inventory.Version{Major: 1, Minor: 34}, day("2026-10-02")
	for name, tc := range map[string]struct {
		inv  inventory.Inventory
		want string
	}{
		"versions differ": {installs("istio", "image", mixed...),
			"Detected Istio in namespace(s): mesh-01 (1.29.1 via image), mesh-02 (1.29.2 via image), mesh-03 (1.29.3 via image), mesh-04 (1.29.4 via image), mesh-05 (1.29.5 via image), mesh-06 (1.29.6 via image), mesh-07 (1.29.7 via image), mesh-08 (1.29.8 via image), mesh-09 (1.29.9 via image), mesh-10 (1.29.10 via image), and 15 more. "},
		"versions match": {installs("istio", "image", same...),
			"Detected Istio version 1.29.2 via image in namespace(s): mesh-01, mesh-02, mesh-03, mesh-04, mesh-05, mesh-06, mesh-07, mesh-08, mesh-09, mesh-10, and 15 more. "},
	} {
		fs := evalAddOns(tc.inv, lifecycleKB(), target, now)
		if len(fs) != 1 {
			t.Fatalf("%s: want one finding, got %q", name, whereSummary(fs))
		}
		if !strings.HasPrefix(fs[0].Detail, tc.want) {
			t.Errorf("%s: detail = %q, want prefix %q", name, fs[0].Detail, tc.want)
		}
		if len(fs[0].Namespaces) != 25 || len(fs[0].Teams) != 25 {
			t.Errorf("%s: %d namespaces, %d teams, want 25 each", name, len(fs[0].Namespaces), len(fs[0].Teams))
		}
	}

	var inv inventory.Inventory
	for i := 1; i <= 12; i++ {
		inv.Nodes = append(inv.Nodes, inventory.NodeInfo{Name: fmt.Sprintf("worker-%02d", i), KubeletVersion: "v1.35.2", ContainerRuntime: "containerd://1.7.20"})
	}
	fs := evalAddOns(inv, runtimeKB("1.37"), inventory.Version{Major: 1, Minor: 36}, now)
	want := "Detected containerd version 1.7.20 on node(s): worker-01, worker-02, worker-03, worker-04, worker-05, worker-06, worker-07, worker-08, worker-09, worker-10, and 2 more. "
	if len(fs) != 1 || !strings.HasPrefix(fs[0].Detail, want) {
		t.Errorf("nodes: got %+v, want one finding with detail prefix %q", fs, want)
	}

	// A release line that cannot run the target, on a different patch in
	// each namespace: the incompatible installs are capped the same way.
	var incompat *Finding
	fs = evalAddOns(installs("istio", "image", mixed...), lifecycleKB(), inventory.Version{Major: 1, Minor: 36}, now)
	for i := range fs {
		if fs[i].Category == CatChartIncompat {
			incompat = &fs[i]
		}
	}
	if incompat == nil {
		t.Fatalf("incompat: no chart-incompat finding in %q", whereSummary(fs))
	}
	want = " Incompatible installs: mesh-01 (1.29.1), mesh-02 (1.29.2), mesh-03 (1.29.3), mesh-04 (1.29.4), mesh-05 (1.29.5), mesh-06 (1.29.6), mesh-07 (1.29.7), mesh-08 (1.29.8), mesh-09 (1.29.9), mesh-10 (1.29.10), and 15 more."
	if !strings.HasSuffix(incompat.Detail, want) {
		t.Errorf("incompat: detail = %q, want suffix %q", incompat.Detail, want)
	}
	if len(incompat.Namespaces) != 25 || len(incompat.Teams) != 25 {
		t.Errorf("incompat: %d namespaces, %d teams, want 25 each", len(incompat.Namespaces), len(incompat.Teams))
	}

	ten := strings.Split("a b c d e f g h i j", " ")
	if got := located(ten); got != "a, b, c, d, e, f, g, h, i, j" {
		t.Errorf("located(10) = %q, want all ten", got)
	}
	if got := located(append(ten, "k")); got != "a, b, c, d, e, f, g, h, i, j, and 1 more" {
		t.Errorf("located(11) = %q, want ten and 1 more", got)
	}
}

// A product-level end of life (ingress-nginx retired as a whole) is one
// finding per add-on, naming every install, not one per release line.
func TestEvalAddOnsProductEOLOncePerAddOn(t *testing.T) {
	inv := installs("ingress-nginx", "chart", "edge=4.7.1", "internal=4.11.2")
	fs := evalAddOns(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 30}, testNow)
	want := []string{"blocker eol-addon/ingress-nginx ns=[edge internal] teams=[edgeteam internalteam]"}
	if got := whereSummary(fs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if want := "Detected ingress-nginx in namespace(s): edge (4.7.1 via chart), internal (4.11.2 via chart). Upstream support has ended."; fs[0].Detail != want {
		t.Errorf("detail = %q, want %q", fs[0].Detail, want)
	}
}

// Without release lines, compat rows still judge each install: only the
// install whose version is out of range is named, though every install
// without lifecycle data shares one info.
func TestEvalAddOnsCompatNamesOnlyIncompatibleInstalls(t *testing.T) {
	inv := installs("external-dns", "image", "dns-a=0.9.0", "dns-b=0.14.2")
	fs := evalAddOns(inv, lifecycleKB(), inventory.Version{Major: 1, Minor: 30}, day("2026-10-02"))
	want := []string{
		"blocker chart-incompat/external-dns ns=[dns-a] teams=[dns-ateam]",
		"info addon-no-data/external-dns ns=[dns-a dns-b] teams=[dns-ateam dns-bteam]",
	}
	if got := whereSummary(fs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if want := "ExternalDNS 0.9.0 supports Kubernetes up to 1.21 (target 1.30)"; fs[0].Title != want {
		t.Errorf("title = %q, want %q", fs[0].Title, want)
	}
	if want := "Detected ExternalDNS in namespace(s): dns-a (0.9.0 via image), dns-b (0.14.2 via image). The registry has no release-line data for this version, so its end of life was not assessed."; fs[1].Detail != want {
		t.Errorf("detail = %q, want %q", fs[1].Detail, want)
	}
	// At 1.34 both are out of range, under different rows: one finding,
	// titled for the older, naming both and each version.
	fs = evalAddOns(inv, lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
	if got := whereSummary(fs[:1]); !reflect.DeepEqual(got, []string{"blocker chart-incompat/external-dns ns=[dns-a dns-b] teams=[dns-ateam dns-bteam]"}) {
		t.Fatalf("got %q", got)
	}
	if want := `Installed version 0.9.0 matches compatibility range "<0.10.0", which supports Kubernetes up to 1.21. Incompatible installs: dns-a (0.9.0), dns-b (0.14.2).`; fs[0].Detail != want {
		t.Errorf("detail = %q, want %q", fs[0].Detail, want)
	}
}

// Split support (#265): support ends for everyone on eol_date and lasts
// until extended_eol_date only under a condition the collector cannot see
// (a vendor subscription). Before eol_date nothing is wrong yet; between
// the two dates the condition decides, so a warning states it; past the
// later date the product is end of life for everyone.
func TestEvalAddOnsSplitSupport(t *testing.T) {
	cond := "the cluster has a SUSE Rancher Prime LTS subscription"
	k := kb.KB{AddOns: []registry.AddOn{{
		SchemaVersion: 2, ID: "vendor-ingress", DisplayName: "Vendor Ingress",
		Matchers: registry.Matchers{Charts: []string{"vendor-ingress"}},
		Support: registry.Support{Status: "supported", EOLDate: "2026-03-31", ExtendedEOLDate: "2027-11-30",
			ExtendedSupportCondition: cond, Citations: []string{"https://docs.rke2.io/reference/ingress_migration"}},
		Recommendation: "Migrate to Traefik.",
	}}}
	target := inventory.Version{Major: 1, Minor: 34}
	inv := addOnAt("vendor-ingress", "1.12.6")
	for _, tc := range []struct {
		now                string
		sev                Severity
		key, title, detail string
	}{
		{now: "2025-12-15"}, // eol_date more than 90 days away
		{"2026-01-15", SevWarning, "eol-approaching/vendor-ingress",
			"Vendor Ingress reaches end-of-life on 2026-03-31",
			"Support ends on 2026-03-31; after that it continues until 2027-11-30 only if " + cond + ", which upgradescope cannot see."},
		{"2026-10-09", SevWarning, "eol-approaching/vendor-ingress",
			"Vendor Ingress is supported until 2027-11-30 only if " + cond,
			"Support without that condition ended on 2026-03-31; upgradescope cannot see whether this cluster meets it."},
		{"2027-09-15", SevWarning, "eol-approaching/vendor-ingress",
			"Vendor Ingress reaches end-of-life on 2027-11-30",
			"Support until then applies only if " + cond + "; without it, support ended on 2026-03-31."},
		{"2027-12-01", SevBlocker, "eol-addon/vendor-ingress",
			"Vendor Ingress is end-of-life since 2027-11-30",
			"Support ended on 2026-03-31, and support that applied only if " + cond + " ended on 2027-11-30."},
	} {
		fs := evalAddOns(inv, k, target, day(tc.now))
		if tc.key == "" {
			if len(fs) != 0 {
				t.Errorf("%s: want no finding, got %s", tc.now, summarize(fs))
			}
			continue
		}
		if len(fs) != 1 {
			t.Fatalf("%s: want one finding, got %s", tc.now, summarize(fs))
		}
		f := fs[0]
		if f.Severity != tc.sev || f.Key != tc.key || f.Title != tc.title || !strings.HasSuffix(f.Detail, " "+tc.detail) {
			t.Errorf("%s: got %s %s %q\n%q\nwant %s %s %q\n...%q", tc.now, f.Severity, f.Key, f.Title, f.Detail, tc.sev, tc.key, tc.title, tc.detail)
		}
		if f.Remediation != "Migrate to Traefik." || !slices.Equal(f.Citations, k.AddOns[0].Support.Citations) {
			t.Errorf("%s: remediation %q citations %v", tc.now, f.Remediation, f.Citations)
		}
	}
}

// The embedded RKE2 Ingress NGINX entry carries SUSE's split support
// (#265): community builds ended in March 2026, Prime LTS support runs
// through November 2027.
func TestEvalAddOnsRKE2IngressNginxSupportEnd(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons, Skew: kb.DefaultSkewPolicy(), MaxKnownK8s: inventory.Version{Major: 1, Minor: 99}}
	target := inventory.Version{Major: 1, Minor: 33}
	inv := addOnAt("rke2-ingress-nginx", "1.12.6")
	for now, want := range map[string]string{
		"2026-10-09": "warning eol-approaching eol-approaching/rke2-ingress-nginx RKE2 Ingress NGINX is supported until 2027-11-30 only if the cluster has a SUSE Rancher Prime LTS subscription",
		"2027-09-15": "warning eol-approaching eol-approaching/rke2-ingress-nginx RKE2 Ingress NGINX reaches end-of-life on 2027-11-30",
		"2027-12-01": "blocker eol-addon eol-addon/rke2-ingress-nginx RKE2 Ingress NGINX is end-of-life since 2027-11-30",
	} {
		if got := strings.Join(summarize(evalAddOns(inv, k, target, day(now))), "\n"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", now, got, want)
		}
	}
}
