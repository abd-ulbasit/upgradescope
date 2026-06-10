// registry/load_test.go
package registry

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func validYAML(id string) string {
	return `schema_version: 1
id: ` + id + `
display_name: Test Add-on
matchers:
  charts:
    - ` + id + `
support:
  status: supported
  citations:
    - https://example.com/releases
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
			files:   map[string]string{"a.yaml": validYAML("addon-a")},
			wantIDs: []string{"addon-a"},
		},
		{
			name:    "entries sorted by id regardless of file name",
			files:   map[string]string{"z.yaml": validYAML("addon-a"), "a.yaml": validYAML("addon-b")},
			wantIDs: []string{"addon-a", "addon-b"},
		},
		{
			name:    "non-yaml files ignored",
			files:   map[string]string{"a.yaml": validYAML("addon-a"), "README.md": "# docs"},
			wantIDs: []string{"addon-a"},
		},
		{
			name:    "duplicate id across files",
			files:   map[string]string{"a.yaml": validYAML("addon-a"), "b.yaml": validYAML("addon-a")},
			wantErr: `duplicate id "addon-a"`,
		},
		{
			name:    "malformed yaml",
			files:   map[string]string{"a.yaml": "{not yaml: ["},
			wantErr: "parse data/a.yaml",
		},
		{
			name:    "unknown field rejected (strict mode)",
			files:   map[string]string{"a.yaml": validYAML("addon-a") + "bogus_field: x\n"},
			wantErr: "unknown field",
		},
		{
			name: "validation failure names the file",
			files: map[string]string{
				"a.yaml": strings.Replace(validYAML("addon-a"), "schema_version: 1", "schema_version: 2", 1),
			},
			wantErr: "data/a.yaml",
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

func TestLoadEmbedded(t *testing.T) {
	addons, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	for _, a := range addons {
		if a.ID == "ingress-nginx" {
			if a.Support.Status != "eol" {
				t.Errorf("ingress-nginx status = %q, want eol", a.Support.Status)
			}
			if a.Support.EOLDate != "2026-03-24" {
				t.Errorf("ingress-nginx eol_date = %q, want 2026-03-24", a.Support.EOLDate)
			}
			return
		}
	}
	t.Fatalf("ingress-nginx not found in embedded registry (%d entries)", len(addons))
}

func TestLoadCuratedEntries(t *testing.T) {
	addons, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	got := map[string]AddOn{}
	for _, a := range addons {
		got[a.ID] = a
	}
	tests := []struct {
		id       string
		status   string
		image    string
		chart    string
		citation string
	}{
		{"ingress-nginx", "eol", "registry.k8s.io/ingress-nginx/controller", "ingress-nginx",
			"https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/"},
		{"cert-manager", "supported", "quay.io/jetstack/cert-manager-controller", "cert-manager",
			"https://cert-manager.io/docs/releases/"},
		{"coredns", "supported", "registry.k8s.io/coredns/coredns", "coredns",
			"https://github.com/coredns/deployment/blob/master/kubernetes/CoreDNS-k8s_version.md"},
		{"metrics-server", "supported", "registry.k8s.io/metrics-server/metrics-server", "metrics-server",
			"https://github.com/kubernetes-sigs/metrics-server#compatibility-matrix"},
		{"kube-state-metrics", "supported", "registry.k8s.io/kube-state-metrics/kube-state-metrics", "kube-state-metrics",
			"https://github.com/kubernetes/kube-state-metrics#compatibility-matrix"},
	}
	if len(addons) != len(tests) {
		t.Errorf("embedded registry has %d entries, want %d", len(addons), len(tests))
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			a, ok := got[tt.id]
			if !ok {
				t.Fatalf("%s not found in embedded registry", tt.id)
			}
			if a.Support.Status != tt.status {
				t.Errorf("status = %q, want %q", a.Support.Status, tt.status)
			}
			if !slices.Contains(a.Matchers.Images, tt.image) {
				t.Errorf("images %v missing %q", a.Matchers.Images, tt.image)
			}
			if !slices.Contains(a.Matchers.Charts, tt.chart) {
				t.Errorf("charts %v missing %q", a.Matchers.Charts, tt.chart)
			}
			if !slices.Contains(a.Support.Citations, tt.citation) {
				t.Errorf("citations %v missing %q", a.Support.Citations, tt.citation)
			}
		})
	}
	// ingress-nginx specifics: the demo centerpiece must carry both citations,
	// the EOL date, and a remediation hint.
	in := got["ingress-nginx"]
	if in.Support.EOLDate != "2026-03-24" {
		t.Errorf("ingress-nginx eol_date = %q, want 2026-03-24", in.Support.EOLDate)
	}
	if !slices.Contains(in.Support.Citations, "https://kubernetes.io/blog/2026/01/29/ingress-nginx-statement/") {
		t.Errorf("ingress-nginx missing 2026-01-29 statement citation, got %v", in.Support.Citations)
	}
	if !strings.Contains(in.Recommendation, "Gateway API") {
		t.Errorf("ingress-nginx recommendation = %q, want Gateway API migration hint", in.Recommendation)
	}
}
