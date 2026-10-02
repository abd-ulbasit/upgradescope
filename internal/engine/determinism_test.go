package engine

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// shuffleInventory reorders every slice of inv whose order carries no
// meaning, nested ones included: the same logical inventory can reach
// Evaluate in any row order (a pusher that orders rows differently, an
// agent from another version), and the report must not change. CRD
// Versions keep spec order, which the collector reports as is.
func shuffleInventory(r *rand.Rand, inv *inventory.Inventory) {
	shuffle := func(n int, swap func(i, j int)) { r.Shuffle(n, swap) }
	usage := func(us []inventory.APIUsage) {
		shuffle(len(us), func(i, j int) { us[i], us[j] = us[j], us[i] })
		for _, u := range us {
			shuffle(len(u.Objects), func(i, j int) { u.Objects[i], u.Objects[j] = u.Objects[j], u.Objects[i] })
		}
	}
	usage(inv.APIUsage)
	shuffle(len(inv.DeprecatedCalls), func(i, j int) {
		inv.DeprecatedCalls[i], inv.DeprecatedCalls[j] = inv.DeprecatedCalls[j], inv.DeprecatedCalls[i]
	})
	shuffle(len(inv.HelmReleases), func(i, j int) { inv.HelmReleases[i], inv.HelmReleases[j] = inv.HelmReleases[j], inv.HelmReleases[i] })
	for _, rel := range inv.HelmReleases {
		usage(rel.ManifestAPIs)
	}
	shuffle(len(inv.AddOns), func(i, j int) { inv.AddOns[i], inv.AddOns[j] = inv.AddOns[j], inv.AddOns[i] })
	for _, a := range inv.AddOns {
		shuffle(len(a.Namespaces), func(i, j int) { a.Namespaces[i], a.Namespaces[j] = a.Namespaces[j], a.Namespaces[i] })
	}
	shuffle(len(inv.Nodes), func(i, j int) { inv.Nodes[i], inv.Nodes[j] = inv.Nodes[j], inv.Nodes[i] })
	shuffle(len(inv.ControlPlane), func(i, j int) { inv.ControlPlane[i], inv.ControlPlane[j] = inv.ControlPlane[j], inv.ControlPlane[i] })
	shuffle(len(inv.Namespaces), func(i, j int) { inv.Namespaces[i], inv.Namespaces[j] = inv.Namespaces[j], inv.Namespaces[i] })
	shuffle(len(inv.UnrecognizedImages), func(i, j int) {
		inv.UnrecognizedImages[i], inv.UnrecognizedImages[j] = inv.UnrecognizedImages[j], inv.UnrecognizedImages[i]
	})
	shuffle(len(inv.CRDs), func(i, j int) { inv.CRDs[i], inv.CRDs[j] = inv.CRDs[j], inv.CRDs[i] })
	for _, c := range inv.CRDs {
		shuffle(len(c.StoredVersions), func(i, j int) { c.StoredVersions[i], c.StoredVersions[j] = c.StoredVersions[j], c.StoredVersions[i] })
		usage(c.Usage)
	}
}

// Evaluate gives the same bytes for the same inventory whatever the order
// of its slices (#165): every golden inventory, shuffled 50 times.
func TestEvaluateOrderInvariant(t *testing.T) {
	kbRaw, err := os.ReadFile("testdata/kb.json")
	if err != nil {
		t.Fatal(err)
	}
	var k kb.KB
	if err := json.Unmarshal(kbRaw, &k); err != nil {
		t.Fatal(err)
	}
	for name, params := range goldenParams {
		t.Run(name, func(t *testing.T) {
			invRaw, err := os.ReadFile(filepath.Join("testdata", name, "inventory.json"))
			if err != nil {
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
			evaluate := func(r *rand.Rand) string {
				var inv inventory.Inventory
				if err := json.Unmarshal(invRaw, &inv); err != nil {
					t.Fatal(err)
				}
				if r != nil {
					shuffleInventory(r, &inv)
				}
				out, err := json.Marshal(Evaluate(inv, k, target, now))
				if err != nil {
					t.Fatal(err)
				}
				return string(out)
			}
			want := evaluate(nil)
			for seed := range uint64(50) {
				if got := evaluate(rand.New(rand.NewPCG(seed, 165))); got != want {
					t.Fatalf("seed %d: report depends on inventory order\n got: %s\nwant: %s", seed, got, want)
				}
			}
		})
	}
}
