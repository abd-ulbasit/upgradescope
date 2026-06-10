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
	for _, e := range entries {
		if !e.IsDir() {
			continue // kb.json
		}
		name := e.Name()
		params, ok := goldenParams[name]
		if !ok {
			t.Fatalf("no goldenParams entry for testdata/%s", name)
		}
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
}
