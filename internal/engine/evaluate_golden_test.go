package engine

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

var update = flag.Bool("update", false, "rewrite golden expected.json files")

// target and now per case; fixed so goldens are stable forever.
var goldenParams = map[string]struct{ target, now string }{
	"clean-cluster":         {"1.34", "2026-06-10T00:00:00Z"},
	"removed-api-at-target": {"1.22", "2026-06-10T00:00:00Z"},
	"eol-ingress-nginx":     {"1.30", "2026-06-10T00:00:00Z"},
	"mixed-everything":      {"1.38", "2026-06-10T00:00:00Z"},
	"degraded-capabilities": {"1.34", "2026-06-10T00:00:00Z"},
	// verdict unknown: a required capability (api-usage) was not assessed.
	"required-capability-missing": {"1.34", "2026-06-10T00:00:00Z"},
	// verdict unknown: target beyond testdata/kb.json MaxKnownK8s (1.36).
	"target-above-horizon": {"1.37", "2026-06-10T00:00:00Z"},
	// GKE GitVersions: the 1.30 node pool is within skew today (3 behind
	// 1.33) but not after the control plane reaches 1.34 → blocker.
	"gke-vendor-versions": {"1.34", "2026-06-10T00:00:00Z"},
	"files-mode":          {"1.36", "2026-06-10T00:00:00Z"},
	// CronJobs written via batch/v1beta1 plus caller rows for the same API:
	// one blocker carrying both; the PDB caller row has no objects and
	// stays a standalone deprecated-api-in-use blocker.
	"removed-api-with-callers": {"1.25", "2026-06-10T00:00:00Z"},
	// Release-line lifecycle: Istio 1.27 (ended) blocks; the containerd 1.7
	// node is EOL but only warns (the node image carries it, and the
	// kubelet keeps 1.x support through 1.37); a containerd 2.0 node is
	// fine; ExternalDNS from Helm is judged by appVersion 0.14.2 (compat
	// row, no lifecycle data → info).
	"addon-lifecycle": {"1.36", "2026-10-02T00:00:00Z"},
}

// canonical re-marshals JSON with sorted keys + fixed indent so byte
// comparison ignores fixture formatting but catches any content drift.
func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out) + "\n"
}

func TestEvaluateGolden(t *testing.T) {
	kbRaw, err := os.ReadFile("testdata/kb.json")
	if err != nil {
		t.Fatal(err)
	}
	var k kb.KB
	if err := json.Unmarshal(kbRaw, &k); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	visited := make(map[string]bool, len(goldenParams))
	for _, e := range entries {
		if !e.IsDir() {
			continue // kb.json
		}
		name := e.Name()
		params, ok := goldenParams[name]
		if !ok {
			t.Fatalf("no goldenParams entry for testdata/%s", name)
		}
		visited[name] = true
		t.Run(name, func(t *testing.T) {
			invRaw, err := os.ReadFile(filepath.Join("testdata", name, "inventory.json"))
			if err != nil {
				t.Fatal(err)
			}
			var inv inventory.Inventory
			if err := json.Unmarshal(invRaw, &inv); err != nil {
				t.Fatal(err)
			}
			target, err := inventory.ParseVersion(params.target)
			if err != nil {
				t.Fatal(err)
			}
			now, err := time.Parse(time.RFC3339, params.now)
			if err != nil {
				t.Fatal(err)
			}

			got := Evaluate(inv, k, target, now)
			gotRaw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			gotJSON := canonical(t, gotRaw)

			goldenPath := filepath.Join("testdata", name, "expected.json")
			if *update {
				if err := os.WriteFile(goldenPath, []byte(gotJSON), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			wantRaw, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if want := canonical(t, wantRaw); gotJSON != want {
				t.Errorf("report mismatch for %s\n got:\n%s\nwant:\n%s", name, gotJSON, want)
			}
		})
	}
	// Reverse check: every declared golden case must have a testdata
	// directory — a deleted/renamed dir must not silently skip its case.
	for name := range goldenParams {
		if !visited[name] {
			t.Errorf("goldenParams entry %q has no testdata/%s directory", name, name)
		}
	}
}
