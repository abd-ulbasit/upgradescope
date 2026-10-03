package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputeWindows(t *testing.T) {
	tests := []struct {
		name    string
		api     string
		want    []windowRow
		wantErr string
	}{
		{
			name: "dates, and a minor that never had extended support",
			api: `[{"cycle":"1.36","eol":"2027-08-02","extendedSupport":"2028-08-02","latest":"1.36-eks-13"},
			       {"cycle":"1.20","eol":"2022-11-01","extendedSupport":false}]`,
			want: []windowRow{
				{minor: "1.36", standardEnd: "2027-08-02", extendedEnd: "2028-08-02"},
				{minor: "1.20", standardEnd: "2022-11-01"},
			},
		},
		{name: "no extendedSupport field", api: `[{"cycle":"1.31","eol":"2025-11-01"}]`, want: []windowRow{{minor: "1.31", standardEnd: "2025-11-01"}}},
		{name: "empty list", api: `[]`, wantErr: "no cycles"},
		{name: "invalid json", api: `{nope`, wantErr: "parse"},
		{name: "no standard end date", api: `[{"cycle":"1.31","eol":false}]`, wantErr: "standard-support end"},
		{name: "unparseable extended date", api: `[{"cycle":"1.31","eol":"2025-11-01","extendedSupport":"soon"}]`, wantErr: "extendedSupport"},
		{name: "extended support true is not a date", api: `[{"cycle":"1.31","eol":"2025-11-01","extendedSupport":true}]`, wantErr: "extendedSupport"},
		{name: "not a Kubernetes minor", api: `[{"cycle":"1.31.2","eol":"2025-11-01"}]`, wantErr: "MAJOR.MINOR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeWindows([]byte(tt.api))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("row %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRenderWindows(t *testing.T) {
	got := renderWindows([]windowRow{
		{minor: "1.36", standardEnd: "2027-08-02", extendedEnd: "2028-08-02"},
		{minor: "1.20", standardEnd: "2022-11-01"},
	})
	want := `versions:
  - {minor: "1.36", standard_end: "2027-08-02", extended_end: "2028-08-02"}
  - {minor: "1.20", standard_end: "2022-11-01"}
`
	if string(got) != want {
		t.Fatalf("renderWindows:\n%s\nwant:\n%s", got, want)
	}
}

const sampleProvider = `# registry/data/providers/eks.yaml
schema_version: 1
id: eks
endoflife_product: amazon-eks
citations:
  - https://endoflife.date/amazon-eks
pricing:
  currency: USD
versions:
  - {minor: "1.20", standard_end: "2022-11-01"}
`

// run also syncs <dir>/providers: the versions block is rewritten, every
// other byte (the hand-entered price included) is kept, and an entry
// without a slug is skipped.
func TestRunSyncsProviders(t *testing.T) {
	dir := t.TempDir()
	prov := filepath.Join(dir, "providers")
	if err := os.Mkdir(prov, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(prov, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("eks.yaml", sampleProvider)
	write("gke.yaml", strings.Replace(sampleProvider, "endoflife_product: amazon-eks\n", "", 1))
	if err := os.WriteFile(filepath.Join(dir, "istio.yaml"), []byte(sampleEntry), 0o644); err != nil {
		t.Fatal(err)
	}
	fetch := func(slug string) ([]byte, error) {
		switch slug {
		case "istio":
			return []byte(`[{"cycle":"1.31","eol":"2027-02-28"}]`), nil
		case "amazon-eks":
			return []byte(`[{"cycle":"1.36","eol":"2027-08-02","extendedSupport":"2028-08-02"}]`), nil
		}
		return nil, errors.New("unexpected slug " + slug)
	}

	var out bytes.Buffer
	drift, err := run(dir, true, fetch, today, &out)
	if err != nil || drift != 2 {
		t.Fatalf("check run: drift=%d err=%v\n%s", drift, err, out.String())
	}
	if raw, _ := os.ReadFile(filepath.Join(prov, "eks.yaml")); string(raw) != sampleProvider {
		t.Fatalf("check mode wrote the file:\n%s", raw)
	}

	drift, err = run(dir, false, fetch, today, &out)
	if err != nil || drift != 2 {
		t.Fatalf("write run: drift=%d err=%v", drift, err)
	}
	want := strings.Replace(sampleProvider, `  - {minor: "1.20", standard_end: "2022-11-01"}`,
		`  - {minor: "1.36", standard_end: "2027-08-02", extended_end: "2028-08-02"}`, 1)
	if raw, _ := os.ReadFile(filepath.Join(prov, "eks.yaml")); string(raw) != want {
		t.Fatalf("eks.yaml after sync:\n%s", raw)
	}
	if drift, err = run(dir, true, fetch, today, &out); err != nil || drift != 0 {
		t.Fatalf("re-check after sync: drift=%d err=%v", drift, err)
	}
}

// A directory with no providers subdirectory is the add-on-only layout.
func TestRunWithoutProvidersDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "istio.yaml"), []byte(sampleEntry+sampleBlock), 0o644); err != nil {
		t.Fatal(err)
	}
	fetch := func(string) ([]byte, error) { return []byte(`[{"cycle":"1.31","eol":"2027-02-28"}]`), nil }
	if drift, err := run(dir, true, fetch, today, &bytes.Buffer{}); err != nil || drift != 0 {
		t.Fatalf("drift=%d err=%v", drift, err)
	}
}
