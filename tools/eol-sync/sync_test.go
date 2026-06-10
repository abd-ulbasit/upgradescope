package main

import (
	"strings"
	"testing"
	"time"
)

var today = time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

func TestComputeSupport(t *testing.T) {
	tests := []struct {
		name       string
		cycles     string // endoflife.date API response (newest cycle first)
		wantStatus string
		wantDate   string
		wantErr    string
	}{
		{
			name:       "newest cycle eol boolean false → supported, empty date",
			cycles:     `[{"cycle":"1.19","eol":false},{"cycle":"1.18","eol":false}]`,
			wantStatus: "supported",
		},
		{
			name:       "newest cycle eol boolean true → eol, empty date",
			cycles:     `[{"cycle":"0.9","eol":true}]`,
			wantStatus: "eol",
		},
		{
			name:       "newest cycle future eol date → supported with date",
			cycles:     `[{"cycle":"1.30","eol":"2026-11-30"},{"cycle":"1.29","eol":"2026-08-31"}]`,
			wantStatus: "supported",
			wantDate:   "2026-11-30",
		},
		{
			name:       "newest cycle past eol date → eol with date",
			cycles:     `[{"cycle":"2.0","eol":"2025-01-01"}]`,
			wantStatus: "eol",
			wantDate:   "2025-01-01",
		},
		{
			name:       "eol date equal to today counts as eol",
			cycles:     `[{"cycle":"3.1","eol":"2026-06-11"}]`,
			wantStatus: "eol",
			wantDate:   "2026-06-11",
		},
		{
			name:       "older cycles being eol does not flip status (newest-cycle rule)",
			cycles:     `[{"cycle":"3.6","eol":false},{"cycle":"3.4","eol":"2020-01-01"}]`,
			wantStatus: "supported",
		},
		{name: "empty cycle list", cycles: `[]`, wantErr: "no cycles"},
		{name: "invalid json", cycles: `{nope`, wantErr: "parse"},
		{name: "unparseable eol date", cycles: `[{"cycle":"1","eol":"soon"}]`, wantErr: "eol date"},
		{name: "eol field of unexpected type", cycles: `[{"cycle":"1","eol":42}]`, wantErr: "eol field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, date, err := computeSupport([]byte(tt.cycles), today)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tt.wantStatus || date != tt.wantDate {
				t.Fatalf("got (%q, %q), want (%q, %q)", status, date, tt.wantStatus, tt.wantDate)
			}
		})
	}
}

const sampleEntry = `# registry/data/istio.yaml
schema_version: 1
id: istio
endoflife_product: istio
matchers:
  images:
    - docker.io/istio
support:
  status: supported
  eol_date: "2026-11-30"
  citations:
    - https://endoflife.date/istio
`

func TestExtractSlug(t *testing.T) {
	if got := extractSlug([]byte(sampleEntry)); got != "istio" {
		t.Fatalf("extractSlug = %q, want istio", got)
	}
	noSlug := strings.Replace(sampleEntry, "endoflife_product: istio\n", "", 1)
	if got := extractSlug([]byte(noSlug)); got != "" {
		t.Fatalf("extractSlug on hand-curated entry = %q, want empty", got)
	}
}

func TestRewriteSupport(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		status  string
		date    string
		want    string // full expected output; "" means expect in unchanged
		wantErr string
	}{
		{
			name:   "no changes needed is byte-identical",
			in:     sampleEntry,
			status: "supported", date: "2026-11-30",
		},
		{
			name:   "status flip preserved everything else",
			in:     sampleEntry,
			status: "eol", date: "2026-11-30",
			want: strings.Replace(sampleEntry, "status: supported", "status: eol", 1),
		},
		{
			name:   "date change rewrites the eol_date line",
			in:     sampleEntry,
			status: "supported", date: "2027-01-15",
			want: strings.Replace(sampleEntry, `eol_date: "2026-11-30"`, `eol_date: "2027-01-15"`, 1),
		},
		{
			name:   "empty date removes the eol_date line",
			in:     sampleEntry,
			status: "supported", date: "",
			want: strings.Replace(sampleEntry, "  eol_date: \"2026-11-30\"\n", "", 1),
		},
		{
			name:   "missing eol_date line is inserted after status",
			in:     strings.Replace(sampleEntry, "  eol_date: \"2026-11-30\"\n", "", 1),
			status: "supported", date: "2026-11-30",
			want: sampleEntry,
		},
		{
			name:    "no support block",
			in:      "schema_version: 1\nid: x\n",
			status:  "supported",
			wantErr: "support",
		},
		{
			name:   "status line outside support block is not touched",
			in:     "top:\n  status: bogus\nsupport:\n  status: supported\n  citations:\n    - https://e.x/\n",
			status: "eol", date: "",
			want: "top:\n  status: bogus\nsupport:\n  status: eol\n  citations:\n    - https://e.x/\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rewriteSupport([]byte(tt.in), tt.status, tt.date)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := tt.want
			if want == "" {
				want = tt.in
			}
			if string(got) != want {
				t.Fatalf("rewriteSupport mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

// Idempotence: applying the same desired state twice changes nothing.
func TestRewriteSupportIdempotent(t *testing.T) {
	once, err := rewriteSupport([]byte(sampleEntry), "eol", "2026-01-01")
	if err != nil {
		t.Fatal(err)
	}
	twice, err := rewriteSupport(once, "eol", "2026-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Fatalf("not idempotent:\n%s\nvs\n%s", once, twice)
	}
}
