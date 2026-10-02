// registry/load_test.go
package registry

import (
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"sigs.k8s.io/yaml"
)

func validYAML(id string) string {
	return `schema_version: 2
id: ` + id + `
display_name: Test Add-on
matchers:
  charts:
    - ` + id + `
support:
  status: supported
  citations:
    - https://vendor.dev/releases
`
}

func TestLoadFS(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantIDs []string
		wantErr string
	}{
		{
			name:    "single valid entry",
			files:   map[string]string{"addon-a.yaml": validYAML("addon-a")},
			wantIDs: []string{"addon-a"},
		},
		{
			name:    "entries sorted by id",
			files:   map[string]string{"addon-b.yaml": validYAML("addon-b"), "addon-a.yaml": validYAML("addon-a")},
			wantIDs: []string{"addon-a", "addon-b"},
		},
		{
			name:    "non-yaml files ignored",
			files:   map[string]string{"addon-a.yaml": validYAML("addon-a"), "README.md": "# docs"},
			wantIDs: []string{"addon-a"},
		},
		{
			// The registry convention is .yaml-only; a .yml file would be
			// silently skipped by the embed glob, so loadFS rejects it loudly.
			name:    "yml extension rejected with rename hint",
			files:   map[string]string{"addon-a.yaml": validYAML("addon-a"), "addon-b.yml": validYAML("addon-b")},
			wantErr: `data/addon-b.yml: registry entries must use the .yaml extension (rename to .yaml)`,
		},
		{
			name:    "id must equal the file name",
			files:   map[string]string{"addon-a.yaml": validYAML("addon-b")},
			wantErr: `data/addon-a.yaml: id "addon-b" must equal the file name (addon-a)`,
		},
		{
			name:    "duplicate id across files",
			files:   map[string]string{"addon-a.yaml": validYAML("addon-a"), "b.yaml": validYAML("addon-a")},
			wantErr: `duplicate id "addon-a"`,
		},
		{
			name:    "malformed yaml",
			files:   map[string]string{"a.yaml": "{not yaml: ["},
			wantErr: "parse data/a.yaml",
		},
		{
			name:    "unknown field rejected (strict mode)",
			files:   map[string]string{"addon-a.yaml": validYAML("addon-a") + "bogus_field: x\n"},
			wantErr: "unknown field",
		},
		{
			name: "validation failure names the file",
			files: map[string]string{
				"addon-a.yaml": strings.Replace(validYAML("addon-a"), "schema_version: 2", "schema_version: 1", 1),
			},
			wantErr: "data/addon-a.yaml",
		},
		{
			name: "cycle eol must be a date or boolean",
			files: map[string]string{
				"addon-a.yaml": validYAML("addon-a") + "cycles:\n  - {cycle: \"1.0\", eol: [1], citations: [\"https://vendor.dev/\"]}\n",
			},
			wantErr: "eol must be a YYYY-MM-DD date, true or false",
		},
		{
			// An empty string would otherwise read as "no end announced".
			name: "cycle eol empty string rejected",
			files: map[string]string{
				"addon-a.yaml": validYAML("addon-a") + "cycles:\n  - {cycle: \"1.0\", eol: \"\", citations: [\"https://vendor.dev/\"]}\n",
			},
			wantErr: "eol must be a YYYY-MM-DD date, true or false",
		},
		{
			// Unquoted, YAML reads 1.10 as the number 1.1: the wrong cycle.
			name: "unquoted cycle rejected",
			files: map[string]string{
				"addon-a.yaml": validYAML("addon-a") + "cycles:\n  - {cycle: 1.10, eol: false, citations: [\"https://vendor.dev/\"]}\n",
			},
			wantErr: `quote versions`,
		},
		{
			name: "unquoted cycle k8s_max rejected",
			files: map[string]string{
				"addon-a.yaml": validYAML("addon-a") + "cycles:\n  - {cycle: \"1.0\", eol: false, k8s_max: 1.30, citations: [\"https://vendor.dev/\"]}\n",
			},
			wantErr: `quote versions`,
		},
		{
			name: "unknown cycle field rejected (strict mode)",
			files: map[string]string{
				"addon-a.yaml": validYAML("addon-a") + "cycles:\n  - {cycle: \"1.0\", eol: false, lts: true, citations: [\"https://vendor.dev/\"]}\n",
			},
			wantErr: "unknown field",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for name, content := range tt.files {
				fsys["data/"+name] = &fstest.MapFile{Data: []byte(content)}
			}
			addons, err := loadFS(fsys, "data")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var ids []string
			for _, a := range addons {
				ids = append(ids, a.ID)
			}
			if !slices.Equal(ids, tt.wantIDs) {
				t.Fatalf("want ids %v, got %v", tt.wantIDs, ids)
			}
		})
	}
}

// Cycle eol values round-trip through YAML and JSON in all three forms
// endoflife.date publishes: a date, true and false.
func TestCycleEOLRoundTrip(t *testing.T) {
	in := `- {cycle: "1.31", eol: "2027-02-28", citations: ["https://endoflife.date/istio"]}
- {cycle: "1.5", eol: true, citations: ["https://endoflife.date/keda"]}
- {cycle: "2.21", eol: false, citations: ["https://endoflife.date/keda"]}
`
	var cycles []Cycle
	if err := yaml.UnmarshalStrict([]byte(in), &cycles); err != nil {
		t.Fatal(err)
	}
	want := []*CycleEOL{{Date: "2027-02-28"}, {Ended: true}, {}}
	for i, c := range cycles {
		if !reflect.DeepEqual(c.EOL, want[i]) {
			t.Errorf("cycles[%d].eol = %+v, want %+v", i, c.EOL, want[i])
		}
	}
	out, err := yaml.Marshal(cycles)
	if err != nil {
		t.Fatal(err)
	}
	var again []Cycle
	if err := yaml.UnmarshalStrict(out, &again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, cycles) {
		t.Fatalf("round trip changed cycles:\n%s", out)
	}
}

// The embedded data files are checked by properties, not by a per-entry
// table: adding a YAML entry needs no Go change, and an eol-sync run that
// moves a date or ends a cycle cannot turn the weekly refresh PR red.
// TestDataDirHoldsOnlyYAMLEntries: go:embed data/*.yaml never sees a .yml
// (or a stray .yaml.bak, a README, a subdirectory), so such a file in the
// source tree is an add-on or edit that CI passes and no binary carries.
// loadFS rejects .yml only in a synthetic filesystem (#166 KB-05).
func TestDataDirHoldsOnlyYAMLEntries(t *testing.T) {
	files, err := os.ReadDir("data")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
			t.Errorf("registry/data/%s: only <id>.yaml files are embedded and loaded; rename it to .yaml or move it out of registry/data", f.Name())
		}
	}
}

func TestEmbeddedEntriesProperties(t *testing.T) {
	addons, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	files, err := fs.Glob(dataFS, "data/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(addons) != len(files) {
		t.Fatalf("Load() returned %d entries for %d data files", len(addons), len(files))
	}
	for _, a := range addons {
		t.Run(a.ID, func(t *testing.T) {
			if a.DisplayName == "" {
				t.Error("display_name is empty")
			}
			if len(a.Support.Citations) == 0 {
				t.Error("support has no citation")
			}
			if a.EndoflifeProduct == "" {
				return
			}
			// Synced entries: lifecycle comes from endoflife.date cycles.
			page := "https://endoflife.date/" + a.EndoflifeProduct
			if !slices.Contains(a.Support.Citations, page) {
				t.Errorf("synced entry must cite %s in support.citations, got %v", page, a.Support.Citations)
			}
			if len(a.Cycles) == 0 {
				t.Error("synced entry has no cycles — run `make eol-sync`")
			}
			for _, c := range a.Cycles {
				if !slices.Contains(c.Citations, page) {
					t.Errorf("cycle %s must cite %s, got %v", c.Cycle, page, c.Citations)
				}
			}
			// A product-level date on a synced entry is the newest-cycle
			// time bomb of v0.1: per-version dates belong in cycles.
			if a.Support.EOLDate != "" {
				t.Errorf("synced entry carries a product-level eol_date %q; per-version dates belong in cycles", a.Support.EOLDate)
			}
		})
	}
}

// ingress-nginx is the README's headline EOL add-on and is hand-curated
// (endoflife.date does not track it), so pinning it cannot conflict with
// eol-sync.
func TestIngressNginxRetirement(t *testing.T) {
	addons, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	i := slices.IndexFunc(addons, func(a AddOn) bool { return a.ID == "ingress-nginx" })
	if i < 0 {
		t.Fatal("ingress-nginx not found in embedded registry")
	}
	in := addons[i]
	if in.Support.Status != "eol" || in.Support.EOLDate != "2026-03-24" {
		t.Errorf("support = %s/%s, want eol/2026-03-24", in.Support.Status, in.Support.EOLDate)
	}
	for _, want := range []string{
		"https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/",
		"https://github.com/kubernetes/ingress-nginx", // archive banner carries the day
	} {
		if !slices.Contains(in.Support.Citations, want) {
			t.Errorf("citations %v missing %q", in.Support.Citations, want)
		}
	}
	if !strings.Contains(in.Recommendation, "Gateway API") {
		t.Errorf("recommendation = %q, want Gateway API migration hint", in.Recommendation)
	}
}
