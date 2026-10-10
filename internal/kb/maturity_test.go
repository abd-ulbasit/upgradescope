package kb

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #332: a remediation never moves a manifest to a less mature API than the
// one it uses (alpha < beta < GA, from the version name). The fallback to
// "the newest version the target serves" used to land on an alpha for a
// beta source, because the KB clamps the introduced minors of old alphas.
// This holds over every shipped entry and every target from 1.16 to the
// KB's horizon, for each way the engine picks a version, ServedAlternative (the
// answer for a manifest at a version the target does not serve yet) included.
func TestRemediationNeverNamesALessMatureVersion(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	idx := NewIndex(k.APILifecycle)
	checked := 0
	for _, e := range k.APILifecycle {
		for minor := 16; minor <= k.MaxKnownK8s.Minor; minor++ {
			target := inventory.Version{Major: 1, Minor: minor}
			check := func(how string, g GVK) {
				checked++
				if stability(g.Version) < stability(e.Version) {
					t.Errorf("%s/%s %s at %s: %s names %s/%s, less mature than %s", e.Group, e.Version, e.Kind, target, how, g.Group, g.Version, e.Version)
				}
			}
			if r, ok := idx.ResolveReplacement(e, target); ok {
				check("ResolveReplacement", r)
			}
			if a, ok := idx.ServedAlternative(e, target); ok {
				check("ServedAlternative", a)
			}
			if s, ok := idx.ServedSuccessor(e, target); ok {
				check("ServedSuccessor", GVK{Group: s.Group, Version: s.Version, Kind: s.Kind})
			}
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d remediations checked over %d entries: the property did not run", checked, len(k.APILifecycle))
	}
}

// A beta source whose GA replacement the target does not serve yet is told
// nothing older: batch/v1beta1 CronJob at 1.18 has no replacement to
// recommend (batch/v1 is served from 1.21; batch/v2alpha1 is an alpha), and
// LaterReplacement names the GA version and its first-served minor.
func TestBetaSourceIsNotSentBackToAnAlpha(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	idx := NewIndex(k.APILifecycle)
	for _, c := range []struct {
		group, version, kind string
		minor                int
	}{
		{"batch", "v1beta1", "CronJob", 18},
		{"discovery.k8s.io", "v1beta1", "EndpointSlice", 17},
		{"", "v1", "Endpoints", 18},
	} {
		e, ok := idx.Lookup(c.group, c.version, c.kind)
		if !ok {
			t.Fatalf("no entry for %s/%s %s", c.group, c.version, c.kind)
		}
		target := inventory.Version{Major: 1, Minor: c.minor}
		if r, ok := idx.ResolveReplacement(e, target); ok && stability(r.Version) < stability(e.Version) {
			t.Errorf("%s/%s %s at %s: ResolveReplacement names %s, less mature than %s", c.group, c.version, c.kind, target, r.Version, c.version)
		}
	}
	cj, _ := idx.Lookup("batch", "v1beta1", "CronJob")
	g, from, ok := idx.LaterReplacement(cj, inventory.Version{Major: 1, Minor: 18})
	if !ok || g.Version != "v1" || from.String() != "1.21" {
		t.Errorf("LaterReplacement(batch/v1beta1 CronJob, 1.18) = %v from %s (%v), want batch/v1 from 1.21", g, from, ok)
	}
}

// The introduced minors that gen-kb's history clamped are corrected (see
// tools/gen-kb introducedFixes), so a 1.16 target no longer reports an API it
// serves as "not served until 1.17", and EndpointSlice's alpha precedes its
// beta.
func TestClampedIntroducedMinorsAreCorrected(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	idx := NewIndex(k.APILifecycle)
	for _, c := range []struct {
		group, version, kind, want string
	}{
		{"discovery.k8s.io", "v1alpha1", "EndpointSlice", "1.16"},
		{"discovery.k8s.io", "v1beta1", "EndpointSlice", "1.17"},
		{"batch", "v2alpha1", "CronJob", "1.5"},
		{"scheduling.k8s.io", "v1alpha1", "PriorityClass", "1.8"},
		{"settings.k8s.io", "v1alpha1", "PodPreset", "1.6"},
	} {
		e, ok := idx.Lookup(c.group, c.version, c.kind)
		if !ok {
			t.Fatalf("no entry for %s/%s %s", c.group, c.version, c.kind)
		}
		if e.Introduced.String() != c.want {
			t.Errorf("%s/%s %s introduced %s, want %s", c.group, c.version, c.kind, e.Introduced, c.want)
		}
		if e.Introduced.Compare(inventory.Version{Major: 1, Minor: 16}) <= 0 && !servedAt(e, inventory.Version{Major: 1, Minor: 16}) && (e.Removed == nil || e.Removed.Minor > 16) {
			t.Errorf("%s/%s %s is not served at 1.16", c.group, c.version, c.kind)
		}
	}
}
