package engine

import (
	"fmt"
	"reflect"
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
		{"version without a cycle", "1.4.2", inventory.Version{Major: 1, Minor: 34},
			[]string{"info addon-no-data addon-no-data/istio no lifecycle data for Istio 1.4.2"}},
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
	fs := evalAddOns(addOnAt("istio", ""), lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
	want := "Detected Istio version (unknown) via image in namespace(s): istio-system. No version could be read from the image tag or chart, so its end of life and Kubernetes compatibility were not assessed."
	if len(fs) != 1 || fs[0].Detail != want || fs[0].Severity != SevInfo {
		t.Fatalf("got %+v, want one info with detail %q", fs, want)
	}
	fs = evalAddOns(addOnAt("istio", "1.4.2"), lifecycleKB(), inventory.Version{Major: 1, Minor: 34}, day("2026-10-02"))
	want = "Detected Istio version 1.4.2 via image in namespace(s): istio-system. The registry has no release-line data for this version, so its end of life was not assessed."
	if len(fs) != 1 || fs[0].Detail != want {
		t.Fatalf("got %+v, want detail %q", fs, want)
	}
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
