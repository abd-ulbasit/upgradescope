// registry/support_test.go
package registry

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func validProvider() ProviderSupport {
	return ProviderSupport{
		SchemaVersion: ProviderSchemaVersion,
		ID:            "eks",
		DisplayName:   "Amazon EKS",
		Citations:     []string{"https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions.html"},
		Versions: []SupportWindow{
			{Minor: "1.34", StandardEnd: "2026-12-02", ExtendedEnd: "2027-12-02"},
			{Minor: "1.20", StandardEnd: "2022-11-01"}, // no extended support was offered
		},
		Pricing: &Pricing{
			Currency:               "USD",
			StandardPerClusterHour: 0.10,
			ExtendedPerClusterHour: 0.60,
			AsOf:                   "2026-10-03",
			Citations:              []string{"https://aws.amazon.com/eks/pricing/"},
		},
	}
}

func TestValidateProvider(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ProviderSupport)
		wantErr string // substring of one returned error; "" means none
	}{
		{"valid entry", func(p *ProviderSupport) {}, ""},
		{"valid entry without pricing", func(p *ProviderSupport) { p.Pricing = nil }, ""},
		{"schema_version", func(p *ProviderSupport) { p.SchemaVersion = 2 }, "schema_version must be 1"},
		{"id must be a provider the inventory names", func(p *ProviderSupport) { p.ID = "oke" }, `id must be one of eks, gke, aks`},
		{"no citation for the dates", func(p *ProviderSupport) { p.Citations = nil }, "at least one citation"},
		{"placeholder citation", func(p *ProviderSupport) { p.Citations = []string{"https://example.com/x"} }, "reserved"},
		{"no versions", func(p *ProviderSupport) { p.Versions = nil }, "at least one version"},
		{"minor must be MAJOR.MINOR", func(p *ProviderSupport) { p.Versions[0].Minor = "1.34.2" }, "minor"},
		{"duplicate minor", func(p *ProviderSupport) { p.Versions[1].Minor = "1.34" }, "duplicate minor"},
		{"standard_end must be a date", func(p *ProviderSupport) { p.Versions[0].StandardEnd = "2026-12" }, "standard_end"},
		{"standard_end is required", func(p *ProviderSupport) { p.Versions[0].StandardEnd = "" }, "standard_end"},
		{"extended_end must be a date", func(p *ProviderSupport) { p.Versions[0].ExtendedEnd = "soon" }, "extended_end"},
		{"extended_end must follow standard_end", func(p *ProviderSupport) { p.Versions[0].ExtendedEnd = "2026-12-02" }, "must be after standard_end"},
		{"price needs a currency", func(p *ProviderSupport) { p.Pricing.Currency = "" }, "currency"},
		{"price only in USD", func(p *ProviderSupport) { p.Pricing.Currency = "EUR" }, "currency"},
		{"price needs an as-of date", func(p *ProviderSupport) { p.Pricing.AsOf = "" }, "as_of"},
		{"as-of must be a date", func(p *ProviderSupport) { p.Pricing.AsOf = "October" }, "as_of"},
		{"price needs a citation", func(p *ProviderSupport) { p.Pricing.Citations = nil }, "pricing: at least one citation"},
		{"standard price must be positive", func(p *ProviderSupport) { p.Pricing.StandardPerClusterHour = 0 }, "standard_per_cluster_hour"},
		{"extended price must exceed standard", func(p *ProviderSupport) { p.Pricing.ExtendedPerClusterHour = 0.10 }, "must exceed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validProvider()
			tt.mutate(&p)
			errs := ValidateProvider(p)
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want no errors, got %v", errs)
				}
				return
			}
			for _, e := range errs {
				if strings.Contains(e.Error(), tt.wantErr) {
					return
				}
			}
			t.Fatalf("want an error containing %q, got %v", tt.wantErr, errs)
		})
	}
}

func providerYAML(id string) string {
	return `schema_version: 1
id: ` + id + `
display_name: Test
citations:
  - https://vendor.dev/versions
versions:
  - {minor: "1.34", standard_end: "2026-12-02", extended_end: "2027-12-02"}
`
}

func TestLoadProvidersFS(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantIDs []string
		wantErr string
	}{
		{"sorted by id", map[string]string{"gke.yaml": providerYAML("gke"), "eks.yaml": providerYAML("eks")}, []string{"eks", "gke"}, ""},
		{"id must equal the file name", map[string]string{"eks.yaml": providerYAML("gke")}, nil, `id "gke" must equal the file name (eks)`},
		{"unknown field rejected", map[string]string{"eks.yaml": providerYAML("eks") + "bogus: 1\n"}, nil, "unknown field"},
		{"unquoted minor rejected", map[string]string{"eks.yaml": strings.Replace(providerYAML("eks"), `minor: "1.34"`, "minor: 1.34", 1)}, nil, "quote versions"},
		{"validation failure names the file", map[string]string{"eks.yaml": strings.Replace(providerYAML("eks"), "schema_version: 1", "schema_version: 9", 1)}, nil, "providers/eks.yaml"},
		{".yml rejected", map[string]string{"eks.yml": providerYAML("eks")}, nil, "must use the .yaml extension"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for name, content := range tt.files {
				fsys["providers/"+name] = &fstest.MapFile{Data: []byte(content)}
			}
			got, err := loadProvidersFS(fsys, "providers")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, p := range got {
				ids = append(ids, p.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tt.wantIDs, ",") {
				t.Fatalf("ids = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}

// The embedded provider dataset loads and has what the engine needs: every
// provider the inventory can name has dates, and EKS, whose price is
// published, carries it.
func TestEmbeddedProviders(t *testing.T) {
	got, err := LoadProviders()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]ProviderSupport{}
	for _, p := range got {
		have[p.ID] = p
	}
	for _, id := range ProviderIDs {
		p, ok := have[id]
		if !ok {
			t.Errorf("no provider dataset for %q", id)
			continue
		}
		if len(p.Versions) == 0 {
			t.Errorf("%s: no versions", id)
		}
	}
	if p := have["eks"]; p.Pricing == nil || p.Pricing.ExtendedPerClusterHour <= p.Pricing.StandardPerClusterHour {
		t.Errorf("eks: the extended-support price is the feature's headline number; got %+v", p.Pricing)
	}
}

// TestProvidersDirHoldsOnlyYAMLEntries is TestDataDirHoldsOnlyYAMLEntries
// for data/providers: go:embed data/providers/*.yaml sees nothing else.
func TestProvidersDirHoldsOnlyYAMLEntries(t *testing.T) {
	files, err := os.ReadDir("data/providers")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name() == ".DS_Store" {
			continue
		}
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
			t.Errorf("registry/data/providers/%s: only <id>.yaml files are embedded and loaded", f.Name())
		}
	}
}
