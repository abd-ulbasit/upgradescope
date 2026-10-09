package inventory

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func conformable() Inventory {
	return Inventory{
		SchemaVersion: 1, CollectorSchema: CurrentCollectorSchema, ServerVersion: "v1.35.2",
		Capabilities: map[Capability]CapabilityStatus{
			CapHelm: {Available: true}, CapAddOns: {Available: true}, CapAPIUsage: {Available: true},
			CapVersions: {Available: true}, CapCRDs: {Available: true},
		},
	}
}

// An inventory with nothing to repair is left exactly as it is.
func TestConformLeavesAGenuineInventoryAlone(t *testing.T) {
	inv := conformable()
	inv.HelmReleases = []HelmRelease{{Name: "shop", Namespace: "apps", ChartName: "shop", ChartVersion: "1.0.0", Status: "deployed",
		ManifestAPIs: []APIUsage{{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 1, Namespaces: map[string]int{"apps": 1},
			Objects: []ObjectRef{{Namespace: "apps", Name: "nightly", Line: 3}}}}}}
	inv.UnrecognizedImages = []string{"docker.io/library/redis"}
	inv.Nodes = []NodeInfo{{Name: "node-1.example.com", KubeletVersion: "v1.35.2"}}
	want := fmt.Sprintf("%+v", inv)
	notes, err := inv.Conform()
	if err != nil || len(notes) != 0 || fmt.Sprintf("%+v", inv) != want {
		t.Errorf("Conform = %q, %v; inventory changed: %v", notes, err, fmt.Sprintf("%+v", inv) != want)
	}
}

// Each shape the collectors copy from objects anyone with access to one
// namespace writes (#268) is repaired so that Admit passes, the repair is
// named in the capability, and the rest of the inventory is kept.
func TestConformRepairsWhatAdmitRefuses(t *testing.T) {
	long := strings.Repeat("a", MaxStringBytes+1)
	for _, tc := range []struct {
		name    string
		mutate  func(*Inventory)
		cap     Capability
		skipped []string
		reason  string
		check   func(*testing.T, Inventory)
	}{
		{"release name", func(inv *Inventory) {
			inv.HelmReleases = append(inv.HelmReleases, HelmRelease{Name: "Bad_Name", Namespace: "apps"})
		}, CapHelm, []string{`apps/"Bad_Name"`}, "name that is not an RFC 1123 subdomain", func(t *testing.T, inv Inventory) {
			if len(inv.HelmReleases) != 1 || inv.HelmReleases[0].Name != "ok" {
				t.Errorf("releases = %+v", inv.HelmReleases)
			}
		}},
		{"release kubeVersion", func(inv *Inventory) {
			inv.HelmReleases = append(inv.HelmReleases, HelmRelease{Name: "big", Namespace: "apps", KubeVersion: long})
		}, CapHelm, []string{"apps/big"}, "chart metadata over", nil},
		{"release chart name", func(inv *Inventory) {
			inv.HelmReleases = append(inv.HelmReleases, HelmRelease{Name: "big", Namespace: "apps", ChartName: long})
		}, CapHelm, []string{"apps/big"}, "chart metadata over", nil},
		{"release name 300 bytes", func(inv *Inventory) {
			inv.HelmReleases = append(inv.HelmReleases, HelmRelease{Name: strings.Repeat("a", 300), Namespace: "apps"})
		}, CapHelm, []string{`apps/"` + strings.Repeat("a", 40) + `…"`}, "name that is not an RFC 1123 subdomain", nil},
		{"manifest object name", func(inv *Inventory) {
			inv.HelmReleases[0].ManifestAPIs = []APIUsage{{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 2,
				Objects: []ObjectRef{{Name: "Bad/Name"}, {Name: "fine"}}}}
		}, CapHelm, []string{"apps/ok"}, "object(s) of API usage dropped for a name that is not an object name", func(t *testing.T, inv Inventory) {
			u := inv.HelmReleases[0].ManifestAPIs[0]
			if len(u.Objects) != 1 || u.ObjectsOmitted != 1 || u.Count != 2 {
				t.Errorf("usage = %+v, want the bad one counted in objectsOmitted", u)
			}
		}},
		{"manifest object namespace", func(inv *Inventory) {
			inv.HelmReleases[0].ManifestAPIs = []APIUsage{{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 1,
				Objects: []ObjectRef{{Namespace: "Not_A_Namespace", Name: "x"}}}}
		}, CapHelm, []string{"apps/ok"}, "namespace that is not a namespace name", nil},
		{"manifest namespace key", func(inv *Inventory) {
			inv.HelmReleases[0].ManifestAPIs = []APIUsage{{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 2,
				Namespaces: map[string]int{"apps": 1, "Bad_NS": 1}}}
		}, CapHelm, []string{}, "namespace key(s) of an API usage count dropped", func(t *testing.T, inv Inventory) {
			if got := inv.HelmReleases[0].ManifestAPIs[0].Namespaces; len(got) != 1 || got["apps"] != 1 {
				t.Errorf("namespaces = %v", got)
			}
		}},
		{"too many objects", func(inv *Inventory) {
			u := APIUsage{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: MaxObjectRefs + 5}
			for i := range MaxObjectRefs + 5 {
				u.Objects = append(u.Objects, ObjectRef{Name: fmt.Sprintf("o%d", i)})
			}
			inv.APIUsage = []APIUsage{u}
		}, CapAPIUsage, []string{}, "beyond the cap counted", func(t *testing.T, inv Inventory) {
			if u := inv.APIUsage[0]; len(u.Objects) != MaxObjectRefs || u.ObjectsOmitted != 5 {
				t.Errorf("usage = %d objects, %d omitted", len(u.Objects), u.ObjectsOmitted)
			}
		}},
		{"live object name", func(inv *Inventory) {
			inv.APIUsage = []APIUsage{{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1, Objects: []ObjectRef{{Name: "a/b"}}}}
		}, CapAPIUsage, []string{}, "object(s) of API usage dropped", nil},
		{"object renderedFrom is cut, not dropped", func(inv *Inventory) {
			inv.HelmReleases[0].ManifestAPIs = []APIUsage{{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 1,
				Objects: []ObjectRef{{Name: "x", RenderedFrom: long}}}}
		}, "", nil, "", func(t *testing.T, inv Inventory) {
			if o := inv.HelmReleases[0].ManifestAPIs[0].Objects; len(o) != 1 || len(o[0].RenderedFrom) > MaxStringBytes || !strings.HasSuffix(o[0].RenderedFrom, cutMark) {
				t.Errorf("objects = %d, renderedFrom %d bytes", len(o), len(o[0].RenderedFrom))
			}
		}},
		{"image repository over the limit", func(inv *Inventory) {
			inv.UnrecognizedImages = []string{"docker.io/library/redis", long}
		}, CapAddOns, []string{}, "image repositor(ies) over", func(t *testing.T, inv Inventory) {
			if !slices.Equal(inv.UnrecognizedImages, []string{"docker.io/library/redis"}) || inv.UnrecognizedImagesOmitted != 1 {
				t.Errorf("images = %q omitted %d", inv.UnrecognizedImages, inv.UnrecognizedImagesOmitted)
			}
		}},
		{"too many images", func(inv *Inventory) {
			for i := range MaxUnrecognizedImages + 3 {
				inv.UnrecognizedImages = append(inv.UnrecognizedImages, fmt.Sprintf("docker.io/library/img%04d", i))
			}
		}, CapAddOns, []string{}, "beyond the cap", func(t *testing.T, inv Inventory) {
			if len(inv.UnrecognizedImages) != MaxUnrecognizedImages || inv.UnrecognizedImagesOmitted != 3 {
				t.Errorf("images %d omitted %d", len(inv.UnrecognizedImages), inv.UnrecognizedImagesOmitted)
			}
		}},
		// The elements Admit still refuses after the above, dropped one at a time.
		{"gitops chart name", func(inv *Inventory) {
			inv.GitOpsCharts = []GitOpsChart{{Tool: GitOpsArgoCD, Name: "Bad_App", Chart: "c"}}
		}, CapHelm, []string{GitOpsArgoCD}, "GitOps chart source(s) dropped", nil},
		{"add-on namespace", func(inv *Inventory) {
			inv.AddOns = []AddOnInstance{{ID: "x", Namespaces: []string{"Bad_NS"}}}
		}, CapAddOns, []string{SkippedPods}, "add-on install(s) dropped", nil},
		{"node name", func(inv *Inventory) {
			inv.Nodes = []NodeInfo{{Name: "Bad_Node"}, {Name: "good"}}
		}, CapVersions, []string{"nodes"}, "node(s) dropped", func(t *testing.T, inv Inventory) {
			if len(inv.Nodes) != 1 || inv.Nodes[0].Name != "good" {
				t.Errorf("nodes = %+v", inv.Nodes)
			}
		}},
		{"namespace team", func(inv *Inventory) {
			inv.Namespaces = []NamespaceInfo{{Name: "apps", Team: strings.Repeat("t", 64)}}
		}, CapVersions, []string{}, "namespace(s) dropped", nil},
		{"provider", func(inv *Inventory) { inv.Provider = Provider(strings.Repeat("p", 64)) }, CapVersions, []string{}, "provider name dropped", func(t *testing.T, inv Inventory) {
			if inv.Provider != "" {
				t.Errorf("provider = %q, want undetermined", inv.Provider)
			}
		}},
		{"api usage namespace key and kind", func(inv *Inventory) {
			inv.APIUsage = []APIUsage{{Group: "g", Version: "v1", Kind: long, Count: 1}}
		}, CapAPIUsage, nil, "API usage entr(ies) dropped", nil},
		{"crd usage", func(inv *Inventory) {
			inv.CRDs = []CRD{{Group: "g", Kind: long, Plural: "p"}}
		}, CapCRDs, []string{}, "CRD(s) dropped", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// (Admit cuts a RenderedFrom itself, as it cuts the other free text.)
			if probe := rebuild(t, tc.mutate); probe.Admit() == nil && tc.cap != "" {
				t.Fatal("test setup: Admit accepts the inventory before Conform")
			}
			inv := rebuild(t, tc.mutate)
			notes, err := inv.Conform()
			if err != nil {
				t.Fatalf("Conform = %v", err)
			}
			if err := inv.Admit(); err != nil {
				t.Fatalf("Admit after Conform = %v", err)
			}
			if tc.cap != "" {
				st := inv.Capabilities[tc.cap]
				if !st.Available || !st.Partial || !strings.Contains(st.Reason, tc.reason) {
					t.Errorf("%s = %+v, want partial, reason with %q", tc.cap, st, tc.reason)
				}
				for _, s := range tc.skipped {
					if !slices.Contains(st.Skipped, s) {
						t.Errorf("%s skipped = %q, want %q in it", tc.cap, st.Skipped, s)
					}
				}
				if len(notes) == 0 {
					t.Error("no notes returned")
				}
			}
			if tc.check != nil {
				tc.check(t, inv)
			}
			// A second pass finds nothing to repair.
			if notes, err := inv.Conform(); err != nil || len(notes) != 0 {
				t.Errorf("second Conform = %q, %v, want nothing", notes, err)
			}
		})
	}
}

func rebuild(t *testing.T, mutate func(*Inventory)) Inventory {
	t.Helper()
	inv := conformable()
	inv.HelmReleases = []HelmRelease{{Name: "ok", Namespace: "apps", ChartName: "ok", ChartVersion: "1.0.0", Status: "deployed"}}
	mutate(&inv)
	return inv
}

// A capability that was already not assessed keeps its status: nothing is
// said to be partial of what was not read at all.
func TestConformKeepsAnUnavailableCapability(t *testing.T) {
	inv := conformable()
	inv.Capabilities[CapHelm] = CapabilityStatus{Reason: "secrets forbidden"}
	inv.HelmReleases = []HelmRelease{{Name: "Bad_Name", Namespace: "apps"}}
	if _, err := inv.Conform(); err != nil {
		t.Fatal(err)
	}
	if st := inv.Capabilities[CapHelm]; st.Available || st.Reason != "secrets forbidden" {
		t.Errorf("helm = %+v, want unchanged", st)
	}
}

// A capability the inventory does not report is created partial and
// available, so what was dropped is not silent.
func TestConformReportsTheCapabilityItRepaired(t *testing.T) {
	inv := Inventory{SchemaVersion: 1, HelmReleases: []HelmRelease{{Name: "Bad_Name", Namespace: "apps"}}}
	if _, err := inv.Conform(); err != nil {
		t.Fatal(err)
	}
	if st := inv.Capabilities[CapHelm]; !st.Available || !st.Partial {
		t.Errorf("helm = %+v, want available and partial", st)
	}
}

// A cluster full of invalid releases names a bounded number of them.
func TestConformBoundsWhatItNames(t *testing.T) {
	inv := conformable()
	for i := range 100 {
		inv.HelmReleases = append(inv.HelmReleases, HelmRelease{Name: fmt.Sprintf("Bad_%03d", i), Namespace: "apps"})
	}
	if _, err := inv.Conform(); err != nil {
		t.Fatal(err)
	}
	st := inv.Capabilities[CapHelm]
	if len(st.Skipped) != maxNamedSkips || !strings.Contains(st.Reason, "80 more not named") || !strings.Contains(st.Reason, "100 Helm release(s) dropped") {
		t.Errorf("helm = %d skipped, reason %q", len(st.Skipped), st.Reason)
	}
	if err := inv.Admit(); err != nil {
		t.Fatal(err)
	}
}

// What Conform does not repair is returned as the error Admit gives.
func TestConformReturnsWhatItCannotRepair(t *testing.T) {
	inv := conformable()
	for i := range MaxCapabilities + 1 {
		inv.Capabilities[Capability(fmt.Sprintf("cap-%d", i))] = CapabilityStatus{Available: true}
	}
	_, err := inv.Conform()
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("Conform = %v, want the capabilities LimitError", err)
	}
}

// The repair names no identifier it dropped as it was: an identifier that
// is not valid is quoted and cut.
func TestConformQuotesAndCutsWhatItNames(t *testing.T) {
	inv := conformable()
	inv.HelmReleases = []HelmRelease{{Name: "<script>alert(1)</script>" + strings.Repeat("x", 1000), Namespace: "apps"}}
	if _, err := inv.Conform(); err != nil {
		t.Fatal(err)
	}
	skipped := inv.Capabilities[CapHelm].Skipped
	if len(skipped) != 1 || len(skipped[0]) > 64 || !strings.HasPrefix(skipped[0], `apps/"<script>`) {
		t.Errorf("skipped = %q", skipped)
	}
	if strings.Contains(inv.Capabilities[CapHelm].Reason, "xxxxxxxxxx") {
		t.Errorf("reason echoes the identifier: %q", inv.Capabilities[CapHelm].Reason)
	}
}
