package engine

import (
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
			if f.Category != CatSupportLifecycle || f.Severity != tt.wantSev || f.Key != "support-lifecycle/eks/1.34" || f.Title != tt.wantTitle {
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
		"other":                  supportInv(inventory.ProviderOther, "v1.34.2"),
		"not determined":         supportInv("", "v1.34.2"),
		"a provider not in KB":   supportInv("oke", "v1.34.2"),
		"minor not in dataset":   supportInv(inventory.ProviderEKS, "v1.99.0-eks-3abc123"),
		"unparseable version":    supportInv(inventory.ProviderEKS, "not-a-version"),
		"no server version":      supportInv(inventory.ProviderEKS, ""),
		"files inventory":        {SchemaVersion: 1, Source: inventory.SourceFiles, Provider: inventory.ProviderEKS, ServerVersion: "v1.34.2"},
		"a minor older than all": supportInv(inventory.ProviderEKS, "v1.10.0-eks-3abc123"),
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
	if got := fs[0].Remediation; !strings.Contains(got, "1.36, 1.35") || strings.Contains(got, "1.34") {
		t.Errorf("remediation = %q, want the newer minors still in standard support, newest first", got)
	}
	// Nothing newer in the dataset: the advice does not name a minor.
	_, fs = evalSupport(supportInv(inventory.ProviderEKS, "v1.36.1-eks-3abc123"), supportKB(), day("2027-07-01"))
	if got := fs[0].Remediation; got == "" || strings.Contains(got, "1.3") {
		t.Errorf("remediation = %q", got)
	}
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
