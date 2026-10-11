package kb

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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
	if _, ok := k.Provider("eks"); !ok || len(k.Providers) != len(registry.ProviderIDs) {
		t.Errorf("Providers = %d entries (eks found: %v), want one per managed provider", len(k.Providers), ok)
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

// TestLoadRefusesHollowLifecycleData: a dataset that parses but holds
// almost nothing, or no removal at all, would judge every manifest ready.
// Load must refuse it itself, not rely on this package's tests running
// against the embedded copy (#166 KB-11).
func TestLoadRefusesHollowLifecycleData(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(apilifecycleJSON, &doc); err != nil {
		t.Fatal(err)
	}
	entries := doc["entries"].([]any)
	variant := func(mutate func([]any) []any) []byte {
		d := map[string]any{"generatedFrom": doc["generatedFrom"], "maxKnownK8s": doc["maxKnownK8s"]}
		cp := make([]any, len(entries))
		for i, e := range entries {
			m := map[string]any{}
			for k, v := range e.(map[string]any) {
				m[k] = v
			}
			cp[i] = m
		}
		d["entries"] = mutate(cp)
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	if _, err := load(apilifecycleJSON); err != nil {
		t.Fatalf("load(embedded) error = %v", err)
	}
	for name, raw := range map[string][]byte{
		"every removal stripped": variant(func(es []any) []any {
			for _, e := range es {
				delete(e.(map[string]any), "removed")
				delete(e.(map[string]any), "removedInferred")
			}
			return es
		}),
		"a single entry": variant(func(es []any) []any { return es[:1] }),
		"a few entries":  variant(func(es []any) []any { return es[:minLifecycleEntries-1] }),
	} {
		_, err := load(raw)
		if err == nil {
			t.Errorf("%s: load() = nil error, want one: the embedded data is corrupt", name)
			continue
		}
		if !strings.Contains(err.Error(), "corrupt") {
			t.Errorf("%s: error = %q, want it to say the embedded data is corrupt", name, err)
		}
	}
}

func TestCheckLifecycleFloors(t *testing.T) {
	file := func(entries, removals int) lifecycleFile {
		f := lifecycleFile{}
		for i := 0; i < entries; i++ {
			e := APILifecycleEntry{Kind: fmt.Sprintf("K%d", i)}
			if i < removals {
				e.Removed = ver(25)
			}
			f.Entries = append(f.Entries, e)
		}
		return f
	}
	cases := []struct {
		entries, removals int
		ok                bool
	}{
		{minLifecycleEntries, minLifecycleRemovals, true},
		{minLifecycleEntries - 1, minLifecycleRemovals, false},
		{minLifecycleEntries, minLifecycleRemovals - 1, false},
		{minLifecycleEntries, 0, false},
		{0, 0, false},
	}
	for _, c := range cases {
		err := checkLifecycleFloors(file(c.entries, c.removals))
		if (err == nil) != c.ok {
			t.Errorf("%d entries, %d with a removal: error = %v, want ok=%v", c.entries, c.removals, err, c.ok)
		}
	}
}

// TestDatasetVersion: the label is derived from content, so any change to
// the lifecycle entries (their migration notes too), the volume plugins or
// the registry yields a different label, and
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
	providers := func() []registry.ProviderSupport {
		return []registry.ProviderSupport{{SchemaVersion: 1, ID: "eks", Versions: []registry.SupportWindow{{Minor: "1.34", StandardEnd: "2026-12-02"}}}}
	}
	const from = "k8s.io/api v0.37.1"

	base, err := datasetVersion(from, entries(), nil, nil, addons(), providers())
	if err != nil {
		t.Fatalf("datasetVersion() error = %v", err)
	}
	again, _ := datasetVersion(from, entries(), nil, nil, addons(), providers())
	if base != again {
		t.Errorf("datasetVersion not deterministic: %q vs %q", base, again)
	}
	// A dataset without built-in groups keeps the label it had before the
	// field existed: the digest is over the bare entries.
	if bare, _ := digest(entries()); !strings.Contains(base, "; lifecycle "+bare+";") {
		t.Errorf("datasetVersion() = %q, want the lifecycle digest %s of the bare entries", base, bare)
	}
	if !strings.HasPrefix(base, from+"; lifecycle ") {
		t.Errorf("datasetVersion() = %q, want prefix %q", base, from+"; lifecycle ")
	}

	synced := addons()
	synced[0].Support.EOLDate = "2027-02-28" // what an eol-sync run changes
	shifted := providers()
	shifted[0].Versions[0].StandardEnd = "2027-01-02" // what an eol-sync run changes
	e := entries()
	e[0].Removed = ver(26)
	tomb := entries()
	tomb[0].RemovedInferred = true

	for name, got := range map[string]func() (string, error){
		"registry eol_date": func() (string, error) { return datasetVersion(from, entries(), nil, nil, synced, providers()) },
		"provider date":     func() (string, error) { return datasetVersion(from, entries(), nil, nil, addons(), shifted) },
		"lifecycle entry":   func() (string, error) { return datasetVersion(from, e, nil, nil, addons(), providers()) },
		"tombstone flag":    func() (string, error) { return datasetVersion(from, tomb, nil, nil, addons(), providers()) },
		"builtin group": func() (string, error) {
			return datasetVersion(from, entries(), []BuiltinGroup{{Group: "imagepolicy.k8s.io", Versions: []string{"v1alpha1"}}}, nil, addons(), providers())
		},
		"migration note": func() (string, error) {
			n := entries()
			n[0].Migration = &Migration{Note: "batch/v1 is a drop-in", Citations: []string{"https://kubernetes.io/docs/reference/using-api/deprecation-guide/"}}
			return datasetVersion(from, n, nil, nil, addons(), providers())
		},
		"volume plugin": func() (string, error) {
			return datasetVersion(from, entries(), nil, VolumePlugins()[:1], addons(), providers())
		},
		"generatedFrom": func() (string, error) {
			return datasetVersion("k8s.io/api v0.37.2", entries(), nil, nil, addons(), providers())
		},
	} {
		v, err := got()
		if err != nil {
			t.Fatalf("%s: datasetVersion() error = %v", name, err)
		}
		if v == base {
			t.Errorf("changing the %s did not change the label %q", name, base)
		}
	}

	// A StorageClass provisioner of a volume plugin (#362) is part of the
	// label: the same plugins with one provisioner edited label otherwise.
	vols := VolumePlugins()
	withVols, _ := datasetVersion(from, entries(), nil, vols, addons(), providers())
	edited := VolumePlugins()
	i := slices.IndexFunc(edited, func(p VolumePlugin) bool { return p.Provisioner != "" })
	if i < 0 {
		t.Fatal("no volume plugin has a provisioner")
	}
	edited[i].Provisioner += "-edited"
	if v, _ := datasetVersion(from, entries(), nil, edited, addons(), providers()); v == withVols {
		t.Errorf("changing a volume plugin's provisioner did not change the label %q", withVols)
	}
}
