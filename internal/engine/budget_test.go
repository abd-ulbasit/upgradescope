package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// goldenInputs returns the golden cases' inventories with their KB,
// target and clock.
func goldenInputs(t *testing.T) (kb.KB, map[string]struct {
	inv    inventory.Inventory
	target inventory.Version
	now    time.Time
}) {
	t.Helper()
	raw, err := os.ReadFile("testdata/kb.json")
	if err != nil {
		t.Fatal(err)
	}
	var k kb.KB
	if err := json.Unmarshal(raw, &k); err != nil {
		t.Fatal(err)
	}
	out := map[string]struct {
		inv    inventory.Inventory
		target inventory.Version
		now    time.Time
	}{}
	for name, p := range goldenParams {
		raw, err := os.ReadFile(filepath.Join("testdata", name, "inventory.json"))
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			inv    inventory.Inventory
			target inventory.Version
			now    time.Time
		}
		if err := json.Unmarshal(raw, &c.inv); err != nil {
			t.Fatal(err)
		}
		if c.target, err = inventory.ParseVersion(p.target); err != nil {
			t.Fatal(err)
		}
		if c.now, err = time.Parse(time.RFC3339, p.now); err != nil {
			t.Fatal(err)
		}
		out[name] = c
	}
	return k, out
}

// Within its limit EvaluateWithin is Evaluate; below what the report
// takes it is ErrReportTooLarge. findingSize stays close to the bytes the
// report's JSON takes, so the limit means about what it says.
func TestEvaluateWithinIsEvaluateWithinItsLimit(t *testing.T) {
	k, cases := goldenInputs(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			want := Evaluate(c.inv, k, c.target, c.now)
			raw, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			got, err := EvaluateWithin(c.inv, k, c.target, c.now, 2*len(raw))
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("EvaluateWithin(2 × %d bytes) = (%d findings, %v), want Evaluate's %d findings",
					len(raw), len(got.Findings), err, len(want.Findings))
			}
			charged := reportBaseSize(c.inv, k)
			for i := range want.Findings {
				charged += findingSize(&want.Findings[i])
			}
			for i := range want.NotAssessed {
				charged += gapSize(&want.NotAssessed[i])
			}
			if charged < len(raw)/2 || charged > 2*len(raw) {
				t.Errorf("charged %d bytes for a %d-byte report, want within a factor of two", charged, len(raw))
			}
			if charged > 0 {
				if _, err := EvaluateWithin(c.inv, k, c.target, c.now, charged-1); !errors.Is(err, ErrReportTooLarge) {
					t.Errorf("EvaluateWithin(%d bytes, one under the charge) = %v, want ErrReportTooLarge", charged-1, err)
				}
			}
		})
	}
}

// totalAlloc is how many bytes f allocates.
func totalAlloc(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// An evaluation over its limit stops as soon as it is, one input
// element after: each check that grows with the inventory charges its
// findings as it builds them. A release per finding, each with a short
// kubeVersion that excludes the target, built a report eight times its
// push.
func TestEvaluateWithinStopsAtItsLimit(t *testing.T) {
	inv := inventory.Inventory{SchemaVersion: 1, ServerVersion: "v1.29.0"}
	for i := range 20000 {
		inv.APIUsage = append(inv.APIUsage, inventory.APIUsage{Group: "batch", Version: "v1beta1", Kind: fmt.Sprint("Kind", i), Count: 1})
		inv.HelmReleases = append(inv.HelmReleases, inventory.HelmRelease{Name: fmt.Sprint("r", i), Namespace: "n", ChartName: "c",
			ChartVersion: "1", KubeVersion: "<1.0", Status: "deployed"})
		inv.DeprecatedCalls = append(inv.DeprecatedCalls, inventory.DeprecatedCall{Group: "g", Version: "v1", Resource: fmt.Sprint("r", i), RemovedRelease: "1.2"})
		inv.CRDs = append(inv.CRDs, inventory.CRD{Group: "example.com", Kind: "Widget", Plural: fmt.Sprint("widgets", i),
			Versions: []inventory.CRDVersion{{Name: "v1", Served: true, Storage: true}}, StoredVersions: []string{"v1", "v0"}})
	}
	target := inventory.Version{Major: 1, Minor: 30}
	var full Report
	all := totalAlloc(func() { full = Evaluate(inv, testKB(), target, testNow) })
	if len(full.Findings) < 4*20000 {
		t.Fatalf("Evaluate built %d findings, want one per usage, call, release and CRD", len(full.Findings))
	}
	for _, only := range []string{"apiUsage", "deprecatedCalls", "helmReleases", "crds"} {
		t.Run(only, func(t *testing.T) {
			one := inventory.Inventory{SchemaVersion: 1, ServerVersion: "v1.29.0"}
			reflect.ValueOf(&one).Elem().FieldByNameFunc(func(n string) bool { return strings.EqualFold(n, only) }).
				Set(reflect.ValueOf(inv).FieldByNameFunc(func(n string) bool { return strings.EqualFold(n, only) }))
			const limit = 64 << 10
			var err error
			used := totalAlloc(func() { _, err = EvaluateWithin(one, testKB(), target, testNow, limit) })
			if !errors.Is(err, ErrReportTooLarge) {
				t.Fatalf("EvaluateWithin(%d bytes) = %v, want ErrReportTooLarge", limit, err)
			}
			if used > all/100 {
				t.Fatalf("stopping at %d bytes allocated %d bytes; the whole evaluation allocates %d", limit, used, all)
			}
		})
	}
}

// The kubelet skew findings name at most maxListedNodes nodes and count
// the rest: 83,000 nodes of 253-byte names fit a push, and each named in
// three findings made reports three times the push.
func TestSkewListsAtMostMaxListedNodes(t *testing.T) {
	inv := inventory.Inventory{ServerVersion: "v1.33.0"}
	for i := range maxListedNodes + 5 {
		inv.Nodes = append(inv.Nodes, inventory.NodeInfo{Name: fmt.Sprintf("node-%03d", i), KubeletVersion: "v1.29.0"})
		inv.Nodes = append(inv.Nodes, inventory.NodeInfo{Name: fmt.Sprintf("bad-%03d", i), KubeletVersion: "x"})
	}
	fs := evalSkew(inv, testKB(), inventory.Version{Major: 1, Minor: 34})
	if len(fs) != 3 {
		t.Fatalf("%d findings, want post-upgrade, current and unparseable: %+v", len(fs), fs)
	}
	for _, f := range fs {
		if !strings.HasPrefix(f.Title, fmt.Sprint(maxListedNodes+5, " node(s)")) || strings.Count(f.Detail, "(") != maxListedNodes ||
			!strings.Contains(f.Detail, ", and 5 more") {
			t.Errorf("%s: title %q, detail %q: want all counted, %d listed and 5 more", f.Key, f.Title, f.Detail, maxListedNodes)
		}
	}
}
