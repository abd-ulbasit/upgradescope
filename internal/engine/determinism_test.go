package engine

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// shuffleInventory reorders every slice of inv whose order carries no
// meaning, nested ones included: the same logical inventory can reach
// Evaluate in any row order (a pusher that orders rows differently, an
// agent from another version, a CRD whose spec.versions was rewritten), and
// the report must not change. That includes a CRD's Versions and a
// capability's Skipped list (#367).
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
		shuffle(len(c.Versions), func(i, j int) { c.Versions[i], c.Versions[j] = c.Versions[j], c.Versions[i] })
		usage(c.Usage)
	}
	for _, st := range inv.Capabilities {
		shuffle(len(st.Skipped), func(i, j int) { st.Skipped[i], st.Skipped[j] = st.Skipped[j], st.Skipped[i] })
	}
	shuffle(len(inv.VolumePlugins), func(i, j int) {
		inv.VolumePlugins[i], inv.VolumePlugins[j] = inv.VolumePlugins[j], inv.VolumePlugins[i]
	})
	for _, v := range inv.VolumePlugins {
		shuffle(len(v.Objects), func(i, j int) { v.Objects[i], v.Objects[j] = v.Objects[j], v.Objects[i] })
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

// A capability's Skipped list is copied into notAssessed[].skipped: two
// entries in either order give the same bytes (#367). No golden has a
// capability with two skipped entries, so the shuffle above cannot see it.
func TestEvaluateOrderInvariantSkipped(t *testing.T) {
	var k kb.KB
	target := inventory.Version{Major: 1, Minor: 34}
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	eval := func(skipped []string) (string, []CapabilityGap) {
		inv := clusterInv()
		inv.Capabilities[inventory.CapHelm] = inventory.CapabilityStatus{
			Available: true, Partial: true, Reason: "two lists failed", Skipped: skipped}
		rep := Evaluate(inv, k, target, now)
		out, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		return string(out), rep.NotAssessed
	}
	a, gaps := eval([]string{"v1 secrets", "apps/v1 deployments"})
	b, _ := eval([]string{"apps/v1 deployments", "v1 secrets"})
	if a != b {
		t.Fatalf("report depends on the order of a capability's Skipped list\n a: %s\n b: %s", a, b)
	}
	var got []string
	for _, g := range gaps {
		if g.Capability == inventory.CapHelm {
			got = g.Skipped
		}
	}
	if want := []string{"apps/v1 deployments", "v1 secrets"}; !slices.Equal(got, want) {
		t.Errorf("notAssessed skipped = %v, want %v (sorted)", got, want)
	}
	// The caller's inventory is not reordered.
	in := []string{"v1 secrets", "apps/v1 deployments"}
	inv := clusterInv()
	inv.Capabilities[inventory.CapHelm] = inventory.CapabilityStatus{Available: true, Partial: true, Skipped: in}
	Evaluate(inv, k, target, now)
	if in[0] != "v1 secrets" {
		t.Errorf("Evaluate sorted the caller's Skipped slice in place: %v", in)
	}
}

// Evaluate leaves the caller's CRD versions in spec order (#367).
func TestEvaluateKeepsCRDVersionOrder(t *testing.T) {
	var k kb.KB
	inv := clusterInv()
	vs := []inventory.CRDVersion{{Name: "v1alpha2", Served: true}, {Name: "v1", Served: true, Storage: true}}
	inv.CRDs = []inventory.CRD{{Group: "g.io", Kind: "K", Plural: "ks", Versions: vs}}
	Evaluate(inv, k, inventory.Version{Major: 1, Minor: 34}, time.Now())
	if vs[0].Name != "v1alpha2" {
		t.Errorf("Evaluate reordered the caller's CRD versions: %v", vs)
	}
}

// A deep shuffle of every JSON array in every golden inventory, 200 seeded
// draws, gives the bytes of the unshuffled one (#367): arrays no typed
// shuffle above names are covered too.
func TestEvaluateDeepShuffleInvariant(t *testing.T) {
	kbRaw, err := os.ReadFile("testdata/kb.json")
	if err != nil {
		t.Fatal(err)
	}
	var k kb.KB
	if err := json.Unmarshal(kbRaw, &k); err != nil {
		t.Fatal(err)
	}
	var deep func(r *rand.Rand, v any)
	deep = func(r *rand.Rand, v any) {
		switch x := v.(type) {
		case []any:
			r.Shuffle(len(x), func(i, j int) { x[i], x[j] = x[j], x[i] })
			for _, e := range x {
				deep(r, e)
			}
		case map[string]any:
			for _, e := range x {
				deep(r, e)
			}
		}
	}
	for name, params := range goldenParams {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name, "inventory.json"))
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
				var generic any
				if err := json.Unmarshal(raw, &generic); err != nil {
					t.Fatal(err)
				}
				if r != nil {
					deep(r, generic)
				}
				b, err := json.Marshal(generic)
				if err != nil {
					t.Fatal(err)
				}
				var inv inventory.Inventory
				if err := json.Unmarshal(b, &inv); err != nil {
					t.Fatal(err)
				}
				out, err := json.Marshal(Evaluate(inv, k, target, now))
				if err != nil {
					t.Fatal(err)
				}
				return string(out)
			}
			want := evaluate(nil)
			for seed := range uint64(200) {
				if got := evaluate(rand.New(rand.NewPCG(seed, 367))); got != want {
					t.Fatalf("seed %d: report depends on array order\n got: %s\nwant: %s", seed, got, want)
				}
			}
		})
	}
}
