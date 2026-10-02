package kb

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// TestLoad asserts invariants of the embedded data rather than its current
// values, so a weekly refresh to a new Kubernetes minor passes unmodified.
func TestLoad(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	f, err := parseLifecycle(apilifecycleJSON)
	if err != nil {
		t.Fatalf("parseLifecycle(embedded) error = %v", err)
	}

	// The horizon is the minor of the k8s.io/api release gen-kb ran
	// against: "k8s.io/api v0.37.1" → 1.37.
	var minor, patch int
	if _, err := fmt.Sscanf(f.GeneratedFrom, "k8s.io/api v0.%d.%d", &minor, &patch); err != nil {
		t.Fatalf("generatedFrom = %q, want \"k8s.io/api v0.<minor>.<patch>\": %v", f.GeneratedFrom, err)
	}
	if want := (inventory.Version{Major: 1, Minor: minor}); k.MaxKnownK8s != want {
		t.Errorf("MaxKnownK8s = %v, want %v (the minor in generatedFrom %q)", k.MaxKnownK8s, want, f.GeneratedFrom)
	}

	label := regexp.MustCompile(`^k8s\.io/api v0\.\d+\.\d+; lifecycle [0-9a-f]{8}; registry [0-9a-f]{8}$`)
	if !label.MatchString(k.Version) || !strings.HasPrefix(k.Version, f.GeneratedFrom+";") {
		t.Errorf("Version = %q, want %q followed by lifecycle and registry digests", k.Version, f.GeneratedFrom)
	}
	if len(k.APILifecycle) == 0 {
		t.Error("APILifecycle is empty")
	}
	if len(k.AddOns) == 0 {
		t.Error("AddOns is empty — registry.Load() returned nothing")
	}
	if k.Skew != DefaultSkewPolicy() {
		t.Errorf("Skew = %+v, want DefaultSkewPolicy()", k.Skew)
	}
	// An upgrade step skips at least one minor and cites the provider
	// documentation that allows it (none ship today).
	for _, s := range k.UpgradeSteps {
		if s.Citation == "" || s.From.Major != s.To.Major || s.To.Minor < s.From.Minor+2 {
			t.Errorf("upgrade step %v → %v (citation %q): want a citation and a step that skips a minor", s.From, s.To, s.Citation)
		}
	}
}

// TestDatasetVersion: the label is derived from content, so any change to
// the lifecycle entries or the registry yields a different label, and
// identical data always yields the same one.
func TestDatasetVersion(t *testing.T) {
	entries := func() []APILifecycleEntry {
		return []APILifecycleEntry{{Group: "batch", Version: "v1beta1", Kind: "CronJob",
			Introduced: inventory.Version{Major: 1, Minor: 8}, Removed: ver(25)}}
	}
	addons := func() []registry.AddOn {
		return []registry.AddOn{{SchemaVersion: 1, ID: "istio",
			Support: registry.Support{Status: "supported", EOLDate: "2026-11-30"}}}
	}
	const from = "k8s.io/api v0.37.1"

	base, err := datasetVersion(from, entries(), addons())
	if err != nil {
		t.Fatalf("datasetVersion() error = %v", err)
	}
	again, _ := datasetVersion(from, entries(), addons())
	if base != again {
		t.Errorf("datasetVersion not deterministic: %q vs %q", base, again)
	}
	if !strings.HasPrefix(base, from+"; lifecycle ") {
		t.Errorf("datasetVersion() = %q, want prefix %q", base, from+"; lifecycle ")
	}

	synced := addons()
	synced[0].Support.EOLDate = "2027-02-28" // what an eol-sync run changes
	e := entries()
	e[0].Removed = ver(26)
	tomb := entries()
	tomb[0].RemovedInferred = true

	for name, got := range map[string]func() (string, error){
		"registry eol_date": func() (string, error) { return datasetVersion(from, entries(), synced) },
		"lifecycle entry":   func() (string, error) { return datasetVersion(from, e, addons()) },
		"tombstone flag":    func() (string, error) { return datasetVersion(from, tomb, addons()) },
		"generatedFrom":     func() (string, error) { return datasetVersion("k8s.io/api v0.37.2", entries(), addons()) },
	} {
		v, err := got()
		if err != nil {
			t.Fatalf("%s: datasetVersion() error = %v", name, err)
		}
		if v == base {
			t.Errorf("changing the %s did not change the label %q", name, base)
		}
	}
}
