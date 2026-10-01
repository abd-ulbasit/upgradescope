package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComputeCycles(t *testing.T) {
	tests := []struct {
		name    string
		api     string // endoflife.date API response (newest cycle first)
		want    []cycleRow
		wantErr string
	}{
		{
			name: "dates, booleans and both Kubernetes-range field spellings",
			api: `[{"cycle":"1.31","eol":"2027-02-28","supportedKubernetesVersions":"1.32 - 1.36"},
			       {"cycle":"1.19","eol":false,"supportedK8sVersions":"1.33 - 1.35"},
			       {"cycle":"1.5","eol":true,"supportedKubernetesVersions":"1.13+"},
			       {"cycle":"3.4","eol":"2025-07-23"}]`,
			want: []cycleRow{
				{cycle: "1.31", eol: `"2027-02-28"`, k8sMin: "1.32", k8sMax: "1.36"},
				{cycle: "1.19", eol: "false", k8sMin: "1.33", k8sMax: "1.35"},
				{cycle: "1.5", eol: "true", k8sMin: "1.13"},
				{cycle: "3.4", eol: `"2025-07-23"`},
			},
		},
		{
			name: "patch-level Kubernetes bound is cut to MAJOR.MINOR",
			api:  `[{"cycle":"1.8","eol":"2023-11-10","supportedK8sVersions":"1.23.3 - 1.25"}]`,
			want: []cycleRow{{cycle: "1.8", eol: `"2023-11-10"`, k8sMin: "1.23", k8sMax: "1.25"}},
		},
		{
			name: "numeric cycle",
			api:  `[{"cycle":2,"eol":false}]`,
			want: []cycleRow{{cycle: "2", eol: "false"}},
		},
		{name: "empty cycle list", api: `[]`, wantErr: "no cycles"},
		{name: "invalid json", api: `{nope`, wantErr: "parse"},
		{name: "unparseable eol date", api: `[{"cycle":"1","eol":"soon"}]`, wantErr: "eol date"},
		{name: "eol field of unexpected type", api: `[{"cycle":"1","eol":42}]`, wantErr: "eol field"},
		{name: "non-numeric cycle", api: `[{"cycle":"focal","eol":false}]`, wantErr: "cycle"},
		{name: "unparseable Kubernetes range", api: `[{"cycle":"1.0","eol":false,"supportedKubernetesVersions":"latest"}]`, wantErr: "supported Kubernetes versions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeCycles([]byte(tt.api))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d rows %+v, want %+v", len(got), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("row %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRenderCycles(t *testing.T) {
	got := renderCycles([]cycleRow{
		{cycle: "1.31", eol: `"2027-02-28"`, k8sMin: "1.32", k8sMax: "1.36"},
		{cycle: "1.5", eol: "true", k8sMin: "1.13"},
		{cycle: "3.4", eol: "false"},
	}, "https://endoflife.date/istio")
	want := `cycles:
  - {cycle: "1.31", eol: "2027-02-28", k8s_min: "1.32", k8s_max: "1.36", citations: ["https://endoflife.date/istio"]}
  - {cycle: "1.5", eol: true, k8s_min: "1.13", citations: ["https://endoflife.date/istio"]}
  - {cycle: "3.4", eol: false, citations: ["https://endoflife.date/istio"]}
`
	if string(got) != want {
		t.Fatalf("renderCycles:\n%s\nwant:\n%s", got, want)
	}
}

const sampleEntry = `# registry/data/istio.yaml
schema_version: 2
id: istio
endoflife_product: istio
matchers:
  images:
    - istio/proxyv2
support:
  status: supported
  citations:
    - https://endoflife.date/istio
`

const sampleBlock = `cycles:
  - {cycle: "1.31", eol: "2027-02-28", citations: ["https://endoflife.date/istio"]}
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

func TestRewriteCycles(t *testing.T) {
	newBlock := "cycles:\n  - {cycle: \"1.32\", eol: false, citations: [\"https://endoflife.date/istio\"]}\n"
	tests := []struct {
		name, in, want string
	}{
		{
			name: "missing block is appended",
			in:   sampleEntry,
			want: sampleEntry + sampleBlock,
		},
		{
			name: "missing trailing newline before appending",
			in:   strings.TrimSuffix(sampleEntry, "\n"),
			want: sampleEntry + sampleBlock,
		},
		{
			name: "block at end of file is replaced",
			in:   sampleEntry + "cycles:\n  - {cycle: \"1.0\", eol: true, citations: [\"https://e.x/\"]}\n  - {cycle: \"0.9\", eol: true, citations: [\"https://e.x/\"]}\n",
			want: sampleEntry + sampleBlock,
		},
		{
			name: "block in the middle is replaced; the keys after it are kept",
			in:   sampleEntry + "cycles:\n  - {cycle: \"1.0\", eol: true, citations: [\"https://e.x/\"]}\ncompat:\n  - range: \"<1.0.0\"\n",
			want: sampleEntry + sampleBlock + "compat:\n  - range: \"<1.0.0\"\n",
		},
		{
			name: "empty flow block is replaced",
			in:   sampleEntry + "cycles: []\n",
			want: sampleEntry + sampleBlock,
		},
		{
			name: "block key with a trailing comment is replaced",
			in:   sampleEntry + "cycles:  # synced\n  - {cycle: \"1.0\", eol: true, citations: [\"https://e.x/\"]}\n",
			want: sampleEntry + sampleBlock,
		},
		{
			name: "CRLF block is replaced",
			in:   sampleEntry + "cycles:\r\n  - {cycle: \"1.0\", eol: true, citations: [\"https://e.x/\"]}\r\ncompat:\r\n",
			want: sampleEntry + sampleBlock + "compat:\r\n",
		},
		{
			// Blank lines and column-0 comments belong to the block when
			// more cycles follow them, and to the next key otherwise.
			name: "blank lines and comments around the block",
			in: sampleEntry + "cycles:\n  - {cycle: \"1.1\", eol: true, citations: [\"https://e.x/\"]}\n\n# old\n" +
				"  - {cycle: \"1.0\", eol: true, citations: [\"https://e.x/\"]}\n\n# hand-curated\ncompat:\n",
			want: sampleEntry + sampleBlock + "\n# hand-curated\ncompat:\n",
		},
		{
			name: "a key that only starts with cycles is not the block",
			in:   sampleEntry + "cycles_note: x\n",
			want: sampleEntry + "cycles_note: x\n" + sampleBlock,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteCycles([]byte(tt.in), []byte(sampleBlock))
			if string(got) != tt.want {
				t.Fatalf("rewriteCycles:\n--- got ---\n%s\n--- want ---\n%s", got, tt.want)
			}
			// Idempotent: applying the same block again changes nothing,
			// and a different block replaces rather than accumulates.
			if again := rewriteCycles(got, []byte(sampleBlock)); !bytes.Equal(again, got) {
				t.Fatalf("not idempotent:\n%s", again)
			}
			if other := rewriteCycles(got, []byte(newBlock)); strings.Count(string(other), "cycles:") != 1 {
				t.Fatalf("second rewrite duplicated the block:\n%s", other)
			}
		})
	}
}

// run end to end against a temp registry dir: drift is reported in check
// mode without writing, then fixed in write mode; support and every other
// byte of the entry are left alone; hand-curated entries are skipped.
func TestRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("istio.yaml", sampleEntry)
	write("velero.yaml", strings.ReplaceAll(strings.Replace(sampleEntry, "endoflife_product: istio\n", "", 1), "istio", "velero"))
	fetch := func(slug string) ([]byte, error) {
		if slug != "istio" {
			return nil, errors.New("unexpected slug " + slug)
		}
		return []byte(`[{"cycle":"1.31","eol":"2027-02-28"}]`), nil
	}

	var out bytes.Buffer
	drift, err := run(dir, true, fetch, today, &out)
	if err != nil || drift != 1 {
		t.Fatalf("check run: drift=%d err=%v", drift, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "istio.yaml")); string(raw) != sampleEntry {
		t.Fatalf("check mode wrote the file:\n%s", raw)
	}
	if !strings.Contains(out.String(), "DRIFT") {
		t.Errorf("check output does not report drift:\n%s", out.String())
	}

	drift, err = run(dir, false, fetch, today, &out)
	if err != nil || drift != 1 {
		t.Fatalf("write run: drift=%d err=%v", drift, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "istio.yaml")); string(raw) != sampleEntry+sampleBlock {
		t.Fatalf("istio.yaml after sync:\n%s", raw)
	}
	if drift, err = run(dir, true, fetch, today, &out); err != nil || drift != 0 {
		t.Fatalf("re-check after sync: drift=%d err=%v", drift, err)
	}
}

// A product whose every cycle has ended may be retired as a whole; eol-sync
// leaves support.status to a human but says so.
func TestRunFlagsAllCyclesEnded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "istio.yaml"), []byte(sampleEntry), 0o644); err != nil {
		t.Fatal(err)
	}
	fetch := func(string) ([]byte, error) {
		return []byte(`[{"cycle":"1.1","eol":"2020-01-01"},{"cycle":"1.0","eol":true}]`), nil
	}
	var out bytes.Buffer
	if _, err := run(dir, false, fetch, today, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "every cycle has ended") {
		t.Fatalf("output does not flag a fully ended product:\n%s", out.String())
	}
}

var today = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
