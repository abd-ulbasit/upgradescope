package engine

import (
	"reflect"
	"testing"
)

func TestSortFindings(t *testing.T) {
	fs := []Finding{
		{Category: CatVersionSkew, Severity: SevWarning, Title: "skew now"},
		{Category: CatDeprecatedAPI, Severity: SevInfo, Title: "info-a"},
		{Category: CatRemovedAPI, Severity: SevBlocker, Title: "z removed"},
		{Category: CatEOLAddon, Severity: SevBlocker, Title: "addon eol"},
		{Category: CatRemovedAPI, Severity: SevBlocker, Title: "a removed"},
		{Category: CatEOLApproaching, Severity: SevWarning, Title: "approaching"},
	}
	sortFindings(fs)
	got := make([][3]string, len(fs))
	for i, f := range fs {
		got[i] = [3]string{string(f.Severity), string(f.Category), f.Title}
	}
	want := [][3]string{
		{"blocker", "eol-addon", "addon eol"},
		{"blocker", "removed-api", "a removed"},
		{"blocker", "removed-api", "z removed"},
		{"warning", "eol-approaching", "approaching"},
		{"warning", "version-skew", "skew now"},
		{"info", "deprecated-api", "info-a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order mismatch:\n got %v\nwant %v", got, want)
	}
}
