package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// largeInventory is a multi-tenant cluster of n namespaces (each owned by
// a team), n Helm releases whose stored manifests use a removed API, n
// CRDs with a deprecated version in use, and an add-on installed in every
// namespace: every place the engine maps namespaces to teams.
func largeInventory(n int) inventory.Inventory {
	inv := inventory.Inventory{SchemaVersion: 1, ServerVersion: "v1.35.0"}
	ingress := map[string]int{}
	for i := range n {
		ns := fmt.Sprintf("ns-%05d", i)
		inv.Namespaces = append(inv.Namespaces, inventory.NamespaceInfo{Name: ns, Team: fmt.Sprintf("team-%03d", i%200)})
		ingress[ns] = 1
		inv.HelmReleases = append(inv.HelmReleases, inventory.HelmRelease{
			Name: fmt.Sprintf("rel-%05d", i), Namespace: ns, ChartName: "app", ChartVersion: "1.0.0", Status: "deployed",
			ManifestAPIs: []inventory.APIUsage{{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1,
				Objects: []inventory.ObjectRef{{Name: "psp", Line: 3}}}},
		})
		inv.CRDs = append(inv.CRDs, inventory.CRD{
			Group: fmt.Sprintf("g%05d.example.com", i), Kind: "Widget", Plural: "widgets",
			Versions: []inventory.CRDVersion{{Name: "v1alpha1", Served: true, Deprecated: true}, {Name: "v1", Served: true, Storage: true}},
			Usage:    []inventory.APIUsage{{Group: fmt.Sprintf("g%05d.example.com", i), Version: "v1alpha1", Kind: "Widget", Count: 1, Namespaces: map[string]int{ns: 1}}},
		})
	}
	inv.APIUsage = []inventory.APIUsage{{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: n, Namespaces: ingress}}
	return inv
}

func benchKB(tb testing.TB) kb.KB {
	tb.Helper()
	k, err := kb.Load()
	if err != nil {
		tb.Fatal(err)
	}
	return k
}

// BenchmarkEvaluateLargeInventory is Evaluate over 5,000 namespaces and
// 5,000 Helm releases and CRDs (#333): the namespace-to-team map was
// rebuilt for each of them, so it grew with their product.
func BenchmarkEvaluateLargeInventory(b *testing.B) {
	for _, n := range []int{1000, 5000, 20000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			inv, k, target := largeInventory(n), benchKB(b), inventory.Version{Major: 1, Minor: 36}
			b.ReportAllocs()
			for b.Loop() {
				Evaluate(inv, k, target, testNow)
			}
		})
	}
}

// bestOf is the shortest of runs timings of f: a loaded machine slows a
// run, never speeds it.
func bestOf(runs int, f func()) time.Duration {
	best := time.Duration(1<<63 - 1)
	for range runs {
		start := time.Now()
		f()
		best = min(best, time.Since(start))
	}
	return best
}

// Evaluate grows linearly with the namespaces (#333). The guard compares
// two sizes of the same inventory rather than a wall-clock time, so a
// slow or busy machine does not trip it: 4x the namespaces cost about 4x
// (less than 9x here), where the quadratic lookup cost 16x. Skipped under
// -short and the race detector, whose slowdown is not uniform.
func TestEvaluateScalesLinearlyWithNamespaces(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("timing guard: skipped under -short and -race")
	}
	k, target := benchKB(t), inventory.Version{Major: 1, Minor: 36}
	small, large := largeInventory(2500), largeInventory(10000)
	Evaluate(small, k, target, testNow) // warm up
	ts := bestOf(3, func() { Evaluate(small, k, target, testNow) })
	tl := bestOf(3, func() { Evaluate(large, k, target, testNow) })
	t.Logf("Evaluate: %s at 2,500 namespaces, %s at 10,000 (%.1fx for 4x)", ts, tl, float64(tl)/float64(ts))
	if tl > 9*ts {
		t.Errorf("Evaluate took %s at 10,000 namespaces and %s at 2,500: %.1fx for 4x the namespaces, want under 9x (linear is about 4x, quadratic 16x)",
			tl, ts, float64(tl)/float64(ts))
	}
}
