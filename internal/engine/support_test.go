package engine

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

func supportKB() kb.KB {
	return kb.KB{Providers: []registry.ProviderSupport{
		{
			SchemaVersion: 1, ID: "eks", DisplayName: "Amazon EKS",
			ExtendedSupportNote: "Extended support is on by default.",
			Citations:           []string{"https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions.html"},
			Versions: []registry.SupportWindow{
				{Minor: "1.36", StandardEnd: "2027-08-02", ExtendedEnd: "2028-08-02"},
				{Minor: "1.35", StandardEnd: "2027-03-27", ExtendedEnd: "2028-03-27"},
				{Minor: "1.34", StandardEnd: "2026-12-02", ExtendedEnd: "2027-12-02"},
				{Minor: "1.20", StandardEnd: "2022-11-01"}, // no extended support was offered
			},
			Pricing: &registry.Pricing{
				Currency: "USD", StandardPerClusterHour: 0.10, ExtendedPerClusterHour: 0.60, AsOf: "2026-10-03",
				Citations: []string{"https://aws.amazon.com/eks/pricing/"},
			},
		},
		{
			SchemaVersion: 1, ID: "gke", DisplayName: "Google Kubernetes Engine",
			Citations: []string{"https://docs.cloud.google.com/kubernetes-engine/docs/release-schedule"},
			Versions:  []registry.SupportWindow{{Minor: "1.34", StandardEnd: "2027-01-25", ExtendedEnd: "2027-11-25"}},
			Pricing: &registry.Pricing{
				Currency: "USD", StandardPerClusterHour: 0.10, ExtendedPerClusterHour: 0.60, AsOf: "2026-10-03",
				Note:      "charged only for clusters on the Extended release channel",
				Citations: []string{"https://cloud.google.com/kubernetes-engine/pricing"},
			},
		},
		{
			// No price: dates only.
			SchemaVersion: 1, ID: "aks", DisplayName: "Azure Kubernetes Service",
			Citations: []string{"https://learn.microsoft.com/en-us/azure/aks/supported-kubernetes-versions"},
			Versions:  []registry.SupportWindow{{Minor: "1.34", StandardEnd: "2026-11-30", ExtendedEnd: "2027-11-30"}},
		},
	}}
}

func supportInv(provider inventory.Provider, server string) inventory.Inventory {
	return inventory.Inventory{SchemaVersion: 1, ClusterID: "c", Provider: provider, ServerVersion: server}
}

func TestEvalSupportPhases(t *testing.T) {
	extFrom := day("2026-12-02")
	tests := []struct {
		name      string
		now       time.Time
		wantPhase SupportPhase
		wantSev   Severity // "" = no finding
		wantTitle string
	}{
		{"long before the window", day("2026-06-10"), SupportStandard, "", ""},
		{"one day outside the window", extFrom.AddDate(0, 0, -91), SupportStandard, "", ""},
		{"first day of the window", extFrom.AddDate(0, 0, -90), SupportEnding, SevWarning,
			"Kubernetes 1.34 leaves Amazon EKS standard support on 2026-12-02"},
		{"inside the window", day("2026-10-15"), SupportEnding, SevWarning,
			"Kubernetes 1.34 leaves Amazon EKS standard support on 2026-12-02"},
		{"the second before extended support", extFrom.Add(-time.Second), SupportEnding, SevWarning,
			"Kubernetes 1.34 leaves Amazon EKS standard support on 2026-12-02"},
		{"the day extended support begins", extFrom, SupportExtended, SevBlocker,
			"Kubernetes 1.34 is past Amazon EKS standard support (ended 2026-12-02)"},
		{"in extended support", day("2027-06-01"), SupportExtended, SevBlocker,
			"Kubernetes 1.34 is past Amazon EKS standard support (ended 2026-12-02)"},
		{"the second before extended support ends", day("2027-12-02").Add(-time.Second), SupportExtended, SevBlocker,
			"Kubernetes 1.34 is past Amazon EKS standard support (ended 2026-12-02)"},
		{"the day extended support ends", day("2027-12-02"), SupportEnded, SevBlocker,
			"Kubernetes 1.34 is out of Amazon EKS support (extended support ended 2027-12-02)"},
		{"long after", day("2029-01-01"), SupportEnded, SevBlocker,
			"Kubernetes 1.34 is out of Amazon EKS support (extended support ended 2027-12-02)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, fs := evalSupport(supportInv(inventory.ProviderEKS, "v1.34.2-eks-3abc123"), supportKB(), tt.now)
			if st == nil || st.Phase != tt.wantPhase {
				t.Fatalf("status = %+v, want phase %s", st, tt.wantPhase)
			}
			if st.Provider != "eks" || st.Minor != "1.34" || st.ExtendedSupportFrom != "2026-12-02" || st.ExtendedSupportEnds != "2027-12-02" {
				t.Errorf("status = %+v", st)
			}
			if tt.wantSev == "" {
				if len(fs) != 0 {
					t.Fatalf("findings = %+v, want none", fs)
				}
				return
			}
			if len(fs) != 1 {
				t.Fatalf("findings = %+v, want one", fs)
			}
			f := fs[0]
			if f.Category != CatSupportLifecycle || f.Severity != tt.wantSev || f.Key != "support-lifecycle/eks/1.34/"+string(tt.wantPhase) || f.Key != SupportKey("eks", "1.34", tt.wantPhase) || f.Title != tt.wantTitle {
				t.Errorf("finding = %s %s %q %q", f.Category, f.Severity, f.Key, f.Title)
			}
		})
	}
}

// The cost is the list price delta, labelled as such with its date, and
// stated only while it can still be paid.
func TestEvalSupportCost(t *testing.T) {
	st, fs := evalSupport(supportInv(inventory.ProviderEKS, "v1.34.2-eks-3abc123"), supportKB(), day("2026-10-15"))
	if st.AnnualCostDelta != "4380.00" || st.Currency != "USD" || st.PriceAsOf != "2026-10-03" {
		t.Errorf("status cost = %q %q %q, want 4380.00 USD 2026-10-03", st.AnnualCostDelta, st.Currency, st.PriceAsOf)
	}
	detail := fs[0].Detail
	for _, want := range []string{
		"2026-12-02", "2027-12-02",
		"$4,380 more per cluster per year at list price",
		"$0.60 against $0.10 per cluster-hour",
		"list price as of 2026-10-03",
		"Extended support is on by default.",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
	for _, c := range []string{
		"https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions.html",
		"https://aws.amazon.com/eks/pricing/",
	} {
		found := false
		for _, got := range fs[0].Citations {
			found = found || got == c
		}
		if !found {
			t.Errorf("citations %v lack %s", fs[0].Citations, c)
		}
	}

	// Before the window there is no finding, but the status carries the date and the figure.
	st, fs = evalSupport(supportInv(inventory.ProviderEKS, "v1.34.2-eks-3abc123"), supportKB(), day("2026-01-01"))
	if len(fs) != 0 || st.AnnualCostDelta != "4380.00" {
		t.Errorf("before the window: findings %v, cost %q", fs, st.AnnualCostDelta)
	}

	// After extended support ends nothing more can be billed for that minor.
	st, fs = evalSupport(supportInv(inventory.ProviderEKS, "v1.34.2-eks-3abc123"), supportKB(), day("2028-01-01"))
	if st.AnnualCostDelta != "" || st.PriceAsOf != "" || strings.Contains(fs[0].Detail, "per cluster per year") {
		t.Errorf("after the end: cost %q as of %q in %q", st.AnnualCostDelta, st.PriceAsOf, fs[0].Detail)
	}
}

func TestEvalSupportNoExtendedSupportOffered(t *testing.T) {
	st, fs := evalSupport(supportInv(inventory.ProviderEKS, "v1.20.15-eks-3abc123"), supportKB(), day("2026-10-15"))
	if st.Phase != SupportEnded || st.ExtendedSupportFrom != "2022-11-01" || st.ExtendedSupportEnds != "" || st.AnnualCostDelta != "" {
		t.Errorf("status = %+v", st)
	}
	if len(fs) != 1 || fs[0].Severity != SevBlocker || fs[0].Title != "Kubernetes 1.20 is out of Amazon EKS support (standard support ended 2022-11-01)" {
		t.Errorf("findings = %+v", fs)
	}
}

// GKE bills extended support only on its Extended channel; the cost line
// says so rather than presenting the figure as every cluster's bill.
func TestEvalSupportPriceNote(t *testing.T) {
	_, fs := evalSupport(supportInv(inventory.ProviderGKE, "v1.34.2-gke.1234000"), supportKB(), day("2027-06-01"))
	if len(fs) != 1 || !strings.Contains(fs[0].Detail, "It is charged only for clusters on the Extended release channel.") {
		t.Errorf("findings = %+v", fs)
	}
}

// A provider whose price is not cited has dates and no cost line, in the
// finding and in the status.
func TestEvalSupportWithoutPrice(t *testing.T) {
	st, fs := evalSupport(supportInv(inventory.ProviderAKS, "v1.34.2"), supportKB(), day("2026-10-15"))
	if st == nil || st.Phase != SupportEnding || st.AnnualCostDelta != "" || st.Currency != "" || st.PriceAsOf != "" {
		t.Fatalf("status = %+v", st)
	}
	if len(fs) != 1 || strings.Contains(fs[0].Detail, "$") || strings.Contains(fs[0].Detail, "list price") {
		t.Errorf("findings = %+v: no cost line without a cited price", fs)
	}
}

// Nothing is said when the provider or the minor is not known: no guess.
func TestEvalSupportUnknown(t *testing.T) {
	now := day("2027-06-01")
	for name, inv := range map[string]inventory.Inventory{
		"other":          supportInv(inventory.ProviderOther, "v1.34.2"),
		"not determined": supportInv("", "v1.34.2"),
		// The engine never infers the provider from the version itself:
		// the collector names it, and an absent or other one is final.
		"not determined, EKS-looking version": supportInv("", "v1.34.2-eks-3abc123"),
		"other, GKE-looking version":          supportInv(inventory.ProviderOther, "v1.34.2-gke.100"),
		"a provider not in KB":                supportInv("oke", "v1.34.2"),
		"minor not in dataset":                supportInv(inventory.ProviderEKS, "v1.99.0-eks-3abc123"),
		"unparseable version":                 supportInv(inventory.ProviderEKS, "not-a-version"),
		"no server version":                   supportInv(inventory.ProviderEKS, ""),
		"files inventory":                     {SchemaVersion: 1, Source: inventory.SourceFiles, Provider: inventory.ProviderEKS, ServerVersion: "v1.34.2"},
		"a minor older than all":              supportInv(inventory.ProviderEKS, "v1.10.0-eks-3abc123"),
	} {
		if st, fs := evalSupport(inv, supportKB(), now); st != nil || len(fs) != 0 {
			t.Errorf("%s: status %+v, findings %+v; want neither", name, st, fs)
		}
	}
	if st, fs := evalSupport(supportInv(inventory.ProviderEKS, "v1.34.2"), kb.KB{}, now); st != nil || len(fs) != 0 {
		t.Errorf("empty KB: %+v %+v", st, fs)
	}
}

func TestEvalSupportRemediationNamesNewerMinorsInStandardSupport(t *testing.T) {
	_, fs := evalSupport(supportInv(inventory.ProviderEKS, "v1.34.2-eks-3abc123"), supportKB(), day("2026-10-15"))
	want := "Upgrade the control plane, one minor at a time. The nearest minor in standard support is 1.35; the newest known is 1.36."
	if got := fs[0].Remediation; got != want {
		t.Errorf("remediation = %q, want %q", got, want)
	}
	// A cluster several minors behind is told the nearest minor still in
	// standard support, not only the newest: its first hop may be past it.
	_, fs = evalSupport(supportInv(inventory.ProviderEKS, "v1.20.15-eks-3abc123"), supportKB(), day("2027-04-01"))
	if got := fs[0].Remediation; !strings.Contains(got, "nearest minor in standard support is 1.36") || strings.Contains(got, "1.35") {
		t.Errorf("remediation at 2027-04-01 = %q, want 1.35 (ended 2027-03-27) skipped for 1.36", got)
	}
	// Nothing newer in the dataset: the advice does not name a minor.
	_, fs = evalSupport(supportInv(inventory.ProviderEKS, "v1.36.1-eks-3abc123"), supportKB(), day("2027-07-01"))
	if got := fs[0].Remediation; got == "" || strings.Contains(got, "1.3") {
		t.Errorf("remediation = %q", got)
	}
}

func TestSupportSummary(t *testing.T) {
	cost := func(s SupportStatus) SupportStatus {
		s.AnnualCostDelta, s.Currency, s.PriceAsOf = "4380.00", "USD", "2026-10-03"
		return s
	}
	base := SupportStatus{Provider: "eks", Minor: "1.34", ExtendedSupportFrom: "2026-12-02", ExtendedSupportEnds: "2027-12-02"}
	for _, tc := range []struct {
		name string
		s    SupportStatus
		want string
	}{
		{"standard, costed", cost(SupportStatus{Provider: "eks", Minor: "1.34", Phase: SupportStandard, ExtendedSupportFrom: "2026-12-02", ExtendedSupportEnds: "2027-12-02"}),
			"EKS 1.34: standard support ends 2026-12-02, extended support until 2027-12-02; extended support adds $4,380/yr per cluster (list price as of 2026-10-03)"},
		{"ending, costed", cost(withPhase(base, SupportEnding)),
			"EKS 1.34: standard support ends 2026-12-02, extended support until 2027-12-02; extended support adds $4,380/yr per cluster (list price as of 2026-10-03)"},
		{"standard, no price", withPhase(SupportStatus{Provider: "aks", Minor: "1.34", ExtendedSupportFrom: "2026-11-30", ExtendedSupportEnds: "2027-11-30"}, SupportStandard),
			"AKS 1.34: standard support ends 2026-11-30, extended support until 2027-11-30"},
		{"extended, opt-in with a note", func() SupportStatus {
			s := cost(withPhase(SupportStatus{Provider: "gke", Minor: "1.34", ExtendedSupportFrom: "2027-01-25", ExtendedSupportEnds: "2027-11-25"}, SupportExtended))
			s.ExtendedSupportCondition, s.AnnualCostNote = "the cluster is on the Extended release channel", "charged only for clusters on the Extended release channel"
			return s
		}(), "GKE 1.34: past standard support since 2027-01-25; extended support until 2027-11-25 (only if the cluster is on the Extended release channel); adds $4,380/yr per cluster (list price as of 2026-10-03, charged only for clusters on the Extended release channel)"},
		{"ending, opt-in, no price", func() SupportStatus {
			s := withPhase(SupportStatus{Provider: "aks", Minor: "1.34", ExtendedSupportFrom: "2026-11-30", ExtendedSupportEnds: "2027-11-30"}, SupportEnding)
			s.ExtendedSupportCondition = "Long Term Support is enabled"
			return s
		}(), "AKS 1.34: standard support ends 2026-11-30, extended support until 2027-11-30 (only if Long Term Support is enabled)"},
		{"extended, costed", cost(withPhase(base, SupportExtended)),
			"EKS 1.34: in extended support since 2026-12-02 (until 2027-12-02); adds $4,380/yr per cluster (list price as of 2026-10-03)"},
		{"ended", withPhase(base, SupportEnded), "EKS 1.34: out of support (extended support ended 2027-12-02)"},
		{"ended, none offered", withPhase(SupportStatus{Provider: "eks", Minor: "1.20", ExtendedSupportFrom: "2022-11-01"}, SupportEnded),
			"EKS 1.20: out of support (standard support ended 2022-11-01)"},
		{"ending, none offered", withPhase(SupportStatus{Provider: "eks", Minor: "1.20", ExtendedSupportFrom: "2026-11-01"}, SupportEnding),
			"EKS 1.20: standard support ends 2026-11-01, no extended support offered"},
	} {
		if got := tc.s.Summary(); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func withPhase(s SupportStatus, p SupportPhase) SupportStatus {
	s.Phase = p
	return s
}

func TestEvalSupportMoney(t *testing.T) {
	for _, tc := range []struct {
		cents int64
		want  string
	}{
		{438000, "$4,380"}, {438050, "$4,380.50"}, {87600, "$876"}, {5, "$0.05"}, {123456789, "$1,234,567.89"}, {100000000, "$1,000,000"},
	} {
		if got := formatUSD(tc.cents); got != tc.want {
			t.Errorf("formatUSD(%d) = %q, want %q", tc.cents, got, tc.want)
		}
	}
}

// Evaluate wires the finding and the status into the report, and a report
// for a cluster whose provider is unknown has neither.
func TestEvaluateSupport(t *testing.T) {
	k := supportKB()
	target := inventory.Version{Major: 1, Minor: 35}
	inv := supportInv(inventory.ProviderEKS, "v1.34.2-eks-3abc123")
	r := Evaluate(inv, k, target, day("2027-06-01"))
	if r.Support == nil || r.Support.Phase != SupportExtended {
		t.Fatalf("Support = %+v", r.Support)
	}
	var found bool
	for _, f := range r.Findings {
		found = found || f.Category == CatSupportLifecycle && f.Severity == SevBlocker
	}
	if !found {
		t.Errorf("no support-lifecycle blocker in %+v", r.Findings)
	}
	inv.Provider = inventory.ProviderOther
	if r := Evaluate(inv, k, target, day("2027-06-01")); r.Support != nil {
		t.Errorf("provider other: Support = %+v", r.Support)
	}
}

// The wording for the two providers whose extended support is opt-in is
// pinned against the real embedded dataset (its notes and conditions), at
// a date inside each phase: the finding states the provider's window
// conditionally, never that this cluster is in it, and the status carries
// the caveats the CRD and the report show beside the date and the price.
func TestEvalSupportOptInProvidersRealDataset(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("aks extended", func(t *testing.T) {
		st, fs := evalSupport(supportInv(inventory.ProviderAKS, "v1.33.4"), k, day("2026-10-04"))
		if st == nil || st.Phase != SupportExtended || st.ExtendedSupportCondition != "Long Term Support is enabled" || st.AnnualCostDelta != "" {
			t.Fatalf("status = %+v", st)
		}
		if got, want := st.Summary(), "AKS 1.33: past standard support since 2026-07-31; extended support until 2027-07-31 (only if Long Term Support is enabled)"; got != want {
			t.Errorf("Summary = %q, want %q", got, want)
		}
		if len(fs) != 1 || fs[0].Severity != SevBlocker {
			t.Fatalf("findings = %+v", fs)
		}
		d := fs[0].Detail
		for _, want := range []string{
			"extended support, which applies only if Long Term Support is enabled, runs until 2027-07-31",
			"cannot see whether this cluster is enrolled",
			"AKS has no automatic extended support.",
			"only platform support",
			"while its minor is N-3, one behind the oldest minor in community support",
			"Older minors are out of support.",
		} {
			if !strings.Contains(d, want) {
				t.Errorf("detail lacks %q:\n%s", want, d)
			}
		}
		// Microsoft limits platform support to N-3; the note must not tell a
		// cluster further behind that it still has it.
		for _, bad := range []string{"the cluster is in extended support", "when Azure Kubernetes Service stops supporting it", "$", "unless Long Term Support is enabled", "a cluster is in platform support"} {
			if strings.Contains(d, bad) {
				t.Errorf("detail asserts %q:\n%s", bad, d)
			}
		}
	})
	t.Run("aks ending", func(t *testing.T) {
		_, fs := evalSupport(supportInv(inventory.ProviderAKS, "v1.34.1"), k, day("2026-10-04"))
		if len(fs) != 1 || fs[0].Severity != SevWarning {
			t.Fatalf("findings = %+v", fs)
		}
		if d := fs[0].Detail; !strings.Contains(d, "extended support, which applies only if Long Term Support is enabled, then runs until 2027-11-30") || strings.Contains(d, "then moves to extended support") {
			t.Errorf("detail = %s", d)
		}
	})
	t.Run("gke ending", func(t *testing.T) {
		st, fs := evalSupport(supportInv(inventory.ProviderGKE, "v1.33.5-gke.1080000"), k, day("2026-06-01"))
		if st == nil || st.Phase != SupportEnding || st.AnnualCostNote != "charged only for clusters on the Extended release channel" || st.AnnualCostDelta != "4380.00" {
			t.Fatalf("status = %+v", st)
		}
		if got, want := st.Summary(), "GKE 1.33: standard support ends 2026-08-12, extended support until 2027-06-12 (only if the cluster is on the Extended release channel); extended support adds $4,380/yr per cluster (list price as of 2026-10-03, charged only for clusters on the Extended release channel)"; got != want {
			t.Errorf("Summary = %q\nwant %q", got, want)
		}
		d := fs[0].Detail
		for _, want := range []string{
			"extended support, which applies only if the cluster is on the Extended release channel, then runs until 2027-06-12",
			"It is charged only for clusters on the Extended release channel.",
			"clusters on other channels are upgraded automatically",
		} {
			if !strings.Contains(d, want) {
				t.Errorf("detail lacks %q:\n%s", want, d)
			}
		}
		if strings.Contains(d, "the cluster then moves to extended support") {
			t.Errorf("detail asserts the move:\n%s", d)
		}
	})
	t.Run("eks is unconditional", func(t *testing.T) {
		st, fs := evalSupport(supportInv(inventory.ProviderEKS, "v1.33.4-eks-3abc123"), k, day("2026-10-04"))
		if st == nil || st.ExtendedSupportCondition != "" || st.AnnualCostNote != "" || len(fs) != 1 {
			t.Fatalf("status = %+v findings %+v", st, fs)
		}
		if !strings.Contains(fs[0].Detail, "the cluster is in extended support until 2027-07-29") || strings.Contains(fs[0].Detail, "only if") {
			t.Errorf("detail = %s", fs[0].Detail)
		}
	})
}

// The support-lifecycle key names the phase, so an accepted or baselined
// finding of one phase never hides a worse later phase (#266): the ending
// warning, the extended-support blocker and the out-of-support blocker are
// three keys.
func TestSupportKeyNamesThePhase(t *testing.T) {
	inv := supportInv(inventory.ProviderEKS, "v1.34.2-eks-3abc123")
	keys := map[SupportPhase]string{}
	for _, c := range []struct {
		now   time.Time
		phase SupportPhase
	}{
		{day("2026-10-15"), SupportEnding},
		{day("2026-12-15"), SupportExtended},
		{day("2027-12-02"), SupportEnded},
	} {
		_, fs := evalSupport(inv, supportKB(), c.now)
		if len(fs) != 1 {
			t.Fatalf("%s: findings = %+v", c.phase, fs)
		}
		keys[c.phase] = fs[0].Key
	}
	want := map[SupportPhase]string{
		SupportEnding:   "support-lifecycle/eks/1.34/ending",
		SupportExtended: "support-lifecycle/eks/1.34/extended",
		SupportEnded:    "support-lifecycle/eks/1.34/ended",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	// Either side of each boundary: the key changes exactly there.
	for _, c := range []struct {
		before, at time.Time
	}{
		{day("2026-12-02").Add(-time.Second), day("2026-12-02")},
		{day("2027-12-02").Add(-time.Second), day("2027-12-02")},
	} {
		_, a := evalSupport(inv, supportKB(), c.before)
		_, b := evalSupport(inv, supportKB(), c.at)
		if a[0].Key == b[0].Key {
			t.Errorf("the key %q is the same either side of %s", a[0].Key, c.at)
		}
	}
}
