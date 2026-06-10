package kb

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestIndexLookup(t *testing.T) {
	entries := []APILifecycleEntry{
		{
			Group:       "extensions",
			Version:     "v1beta1",
			Kind:        "Ingress",
			Introduced:  inventory.Version{Major: 1, Minor: 1},
			Deprecated:  &inventory.Version{Major: 1, Minor: 14},
			Removed:     &inventory.Version{Major: 1, Minor: 22},
			Replacement: &GVK{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"},
		},
		{Group: "", Version: "v1", Kind: "Pod", Introduced: inventory.Version{Major: 1, Minor: 0}},
	}
	idx := NewIndex(entries)

	e, ok := idx.Lookup("extensions", "v1beta1", "Ingress")
	if !ok {
		t.Fatal("Lookup(extensions/v1beta1 Ingress) = not found, want found")
	}
	if e.Removed == nil || *e.Removed != (inventory.Version{Major: 1, Minor: 22}) {
		t.Errorf("Removed = %v, want 1.22", e.Removed)
	}
	if e.Replacement == nil || e.Replacement.Group != "networking.k8s.io" || e.Replacement.Kind != "Ingress" {
		t.Errorf("Replacement = %v, want networking.k8s.io/v1 Ingress", e.Replacement)
	}

	// Core group is the empty string.
	if _, ok := idx.Lookup("", "v1", "Pod"); !ok {
		t.Error("Lookup(core/v1 Pod) = not found, want found")
	}
	// Absent GVK.
	if _, ok := idx.Lookup("apps", "v1", "Deployment"); ok {
		t.Error("Lookup(apps/v1 Deployment) = found, want not found")
	}
}

func TestParseLifecycleFile(t *testing.T) {
	raw := []byte(`{
		"generatedFrom": "k8s.io/api v0.36.1",
		"maxKnownK8s": "1.36",
		"entries": [
			{"group":"batch","version":"v1beta1","kind":"CronJob",
			 "introduced":"1.8",
			 "deprecated":"1.21",
			 "removed":"1.25",
			 "replacement":{"group":"batch","version":"v1","kind":"CronJob"}}
		]
	}`)
	f, err := parseLifecycle(raw)
	if err != nil {
		t.Fatalf("parseLifecycle() error = %v", err)
	}
	if f.MaxKnownK8s != "1.36" {
		t.Errorf("MaxKnownK8s = %q, want 1.36", f.MaxKnownK8s)
	}
	if len(f.Entries) != 1 || f.Entries[0].Kind != "CronJob" {
		t.Errorf("Entries = %+v, want one CronJob entry", f.Entries)
	}
	if f.Entries[0].Introduced != (inventory.Version{Major: 1, Minor: 8}) {
		t.Errorf("Introduced = %v, want 1.8", f.Entries[0].Introduced)
	}
	if f.Entries[0].Removed == nil || *f.Entries[0].Removed != (inventory.Version{Major: 1, Minor: 25}) {
		t.Errorf("Removed = %v, want 1.25", f.Entries[0].Removed)
	}

	// Legacy datasets (written before the canonical string form) encoded
	// versions as {"Major":1,"Minor":25} objects — those must keep parsing.
	legacy := []byte(`{
		"generatedFrom": "k8s.io/api v0.36.1",
		"maxKnownK8s": "1.36",
		"entries": [
			{"group":"batch","version":"v1beta1","kind":"CronJob",
			 "introduced":{"Major":1,"Minor":8},
			 "deprecated":{"Major":1,"Minor":21},
			 "removed":{"Major":1,"Minor":25}}
		]
	}`)
	lf, err := parseLifecycle(legacy)
	if err != nil {
		t.Fatalf("parseLifecycle(legacy object form) error = %v", err)
	}
	if lf.Entries[0].Removed == nil || *lf.Entries[0].Removed != (inventory.Version{Major: 1, Minor: 25}) {
		t.Errorf("legacy Removed = %v, want 1.25", lf.Entries[0].Removed)
	}

	if _, err := parseLifecycle([]byte(`{not json`)); err == nil {
		t.Error("parseLifecycle(corrupt) = nil error, want error")
	}
	if _, err := parseLifecycle([]byte(`{"maxKnownK8s":"","entries":[]}`)); err == nil {
		t.Error("parseLifecycle(empty fields) = nil error, want error")
	}
}
