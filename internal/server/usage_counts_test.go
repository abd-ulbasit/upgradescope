package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #373: a count is what a collector counted, so a push with a negative
// count, objectsOmitted or namespace count in any list of API usage
// entries is refused (422) naming the field, as one in a volume plugin
// entry is, and nothing is stored for the cluster. Before, it was accepted
// (202): the findings read "(-5 objects)", and a negative Helm manifest row
// was dropped by the engine without a finding, so a `helm upgrade` blocker
// vanished (verdict ready, score 100). A count of 0 is still accepted.
func TestIngestRefusesNegativeUsageCounts(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	psp := func() inventory.APIUsage {
		return inventory.APIUsage{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 2, Namespaces: map[string]int{"shop": 2}}
	}
	lists := []struct {
		name  string
		at    string
		build func(inventory.APIUsage) inventory.Inventory
	}{
		{"apiUsage", "apiUsage[0]", func(u inventory.APIUsage) inventory.Inventory {
			inv := testInventory()
			inv.APIUsage = []inventory.APIUsage{u}
			return inv
		}},
		{"apiAuthorshipUnknown", "apiAuthorshipUnknown[0]", func(u inventory.APIUsage) inventory.Inventory {
			inv := testInventory()
			inv.APIAuthorshipUnknown = []inventory.APIUsage{u}
			return inv
		}},
		{"crds usage", "crds[0].usage[0]", func(u inventory.APIUsage) inventory.Inventory {
			inv := testInventory()
			inv.CRDs = []inventory.CRD{{Group: "example.com", Kind: "Widget", Usage: []inventory.APIUsage{u}}}
			return inv
		}},
		{"helm manifestApis", "helmReleases[0].manifestApis[0]", func(u inventory.APIUsage) inventory.Inventory {
			inv := testInventory()
			inv.HelmReleases = []inventory.HelmRelease{{Name: "r", Namespace: "shop", ChartName: "shop", ChartVersion: "1.0.0", Status: "deployed", ManifestAPIs: []inventory.APIUsage{u}}}
			return inv
		}},
	}
	fields := []struct {
		name, suffix string
		set          func(*inventory.APIUsage, int)
	}{
		{"count", ".count", func(u *inventory.APIUsage, n int) { u.Count = n }},
		{"objectsOmitted", ".objectsOmitted", func(u *inventory.APIUsage, n int) { u.ObjectsOmitted = n }},
		{"namespace count", ".namespaces", func(u *inventory.APIUsage, n int) { u.Namespaces["shop"] = n }},
	}
	for _, l := range lists {
		for _, f := range fields {
			t.Run(l.name+"/"+f.name, func(t *testing.T) {
				cluster := "neg-" + strings.ToLower(strings.NewReplacer(" ", "-", "/", "-").Replace(l.name+"-"+f.name))
				u := psp()
				f.set(&u, -5)
				code, out := h.pushAs(cluster, "v0.2.0", l.build(u))
				msg, _ := out["error"].(string)
				if want := l.at + f.suffix; code != http.StatusUnprocessableEntity || !strings.Contains(msg, want) {
					t.Fatalf("push of -5 = %d %v, want 422 naming %s", code, out, want)
				}
				cs, err := h.st.ListClusters(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range cs {
					if c.Name == cluster {
						t.Errorf("cluster %s stored after a refused push", cluster)
					}
				}
				u = psp()
				f.set(&u, 0)
				if code, out := h.pushAs(cluster, "v0.2.0", l.build(u)); code != http.StatusAccepted {
					t.Errorf("push of 0 = %d %v, want 202", code, out)
				}
			})
		}
	}
}
