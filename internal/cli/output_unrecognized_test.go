package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// unrecognizedReport has 12 unrecognized image repositories: 11 listed,
// one dropped by the collector's cap.
func unrecognizedReport() engine.Report {
	r := engine.Report{
		ClusterID: "c",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     100,
		Ready:     true,
		Verdict:   engine.VerdictReady,
	}
	for i := range 11 {
		r.UnrecognizedImages = append(r.UnrecognizedImages, fmt.Sprintf("corp.example/app-%02d", i))
	}
	r.UnrecognizedImagesOmitted = 1
	return r
}

// #18: images no registry entry recognises are listed after the findings,
// bounded, with their total count, so a detection gap is not silent.
func TestWriteTableUnrecognizedImages(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTable(&buf, unrecognizedReport()); err != nil {
		t.Fatal(err)
	}
	want := `
No findings.

UNRECOGNIZED IMAGES (12)
  No add-on image matcher claims these repositories, so an add-on running one is found only through its labels or Helm release:
  - corp.example/app-00
  - corp.example/app-01
  - corp.example/app-02
  - corp.example/app-03
  - corp.example/app-04
  - corp.example/app-05
  - corp.example/app-06
  - corp.example/app-07
  - corp.example/app-08
  - corp.example/app-09
  …and 2 more (--output json lists up to 200)
`
	if got := buf.String(); !strings.HasSuffix(got, want) {
		t.Errorf("table output mismatch\n--- got ---\n%s\n--- want suffix ---\n%s", got, want)
	}

	buf.Reset()
	r := unrecognizedReport()
	r.UnrecognizedImages, r.UnrecognizedImagesOmitted = r.UnrecognizedImages[:1], 0
	if err := WriteTable(&buf, r); err != nil {
		t.Fatal(err)
	}
	if want := "UNRECOGNIZED IMAGES (1)\n  No add-on image matcher claims these repositories, so an add-on running one is found only through its labels or Helm release:\n  - corp.example/app-00\n"; !strings.HasSuffix(buf.String(), want) {
		t.Errorf("table lacks %q:\n%s", want, buf.String())
	}

	buf.Reset()
	if err := WriteTable(&buf, engine.Report{Target: inventory.Version{Major: 1, Minor: 36}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "UNRECOGNIZED") {
		t.Errorf("section rendered without unrecognized images:\n%s", buf.String())
	}
}

func TestWriteMarkdownUnrecognizedImages(t *testing.T) {
	r := unrecognizedReport()
	r.UnrecognizedImages[0] = "corp.example/a`b|c" // manifest text: escaped
	var buf bytes.Buffer
	WriteMarkdown(&buf, r)
	var items strings.Builder
	items.WriteString("- ``corp.example/a`b\\|c``\n")
	for i := 1; i < 10; i++ {
		fmt.Fprintf(&items, "- `corp.example/app-%02d`\n", i)
	}
	want := "\n<details><summary>Unrecognized images (12)</summary>\n" +
		"\n" +
		"No add-on image matcher claims these repositories, so an add-on running one is found only through its labels or Helm release.\n" +
		"\n" +
		items.String() +
		"- …and 2 more (`--output json` lists up to 200)\n" +
		"\n" +
		"</details>\n"
	if got := buf.String(); !strings.HasSuffix(got, want) {
		t.Errorf("markdown output mismatch\n--- got ---\n%s\n--- want suffix ---\n%s", got, want)
	}

	buf.Reset()
	r.UnrecognizedImages, r.UnrecognizedImagesOmitted = nil, 0
	WriteMarkdown(&buf, r)
	if strings.Contains(buf.String(), "Unrecognized") {
		t.Errorf("section rendered without unrecognized images:\n%s", buf.String())
	}
}

func TestWriteJSONUnrecognizedImages(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, unrecognizedReport()); err != nil {
		t.Fatal(err)
	}
	var got struct {
		UnrecognizedImages        []string `json:"unrecognizedImages"`
		UnrecognizedImagesOmitted int      `json:"unrecognizedImagesOmitted"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.UnrecognizedImages, unrecognizedReport().UnrecognizedImages) || got.UnrecognizedImagesOmitted != 1 {
		t.Errorf("JSON carries %v, omitted %d", got.UnrecognizedImages, got.UnrecognizedImagesOmitted)
	}
}
