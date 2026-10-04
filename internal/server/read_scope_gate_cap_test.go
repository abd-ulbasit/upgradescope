package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// capInventory is a cluster with payments' two Ingresses at
// extensions/v1beta1 in pay-prod. With web, team web also has
// inventory.MaxObjectRefs of them in a-web, which sorts before pay-prod:
// a live List returns them first, so the collector lists web's and
// counts payments' two as omitted, as it does on a cluster.
func capInventory(web bool) inventory.Inventory {
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay-prod", Team: "payments"}}
	ingress := inventory.APIUsage{
		Group: "extensions", Version: "v1beta1", Kind: "Ingress", Count: 2, Namespaces: map[string]int{"pay-prod": 2},
		Objects: []inventory.ObjectRef{{Namespace: "pay-prod", Name: "p0"}, {Namespace: "pay-prod", Name: "p1"}},
	}
	if web {
		inv.Namespaces = append([]inventory.NamespaceInfo{{Name: "a-web", Team: "web"}}, inv.Namespaces...)
		ingress.Count += inventory.MaxObjectRefs
		ingress.Namespaces["a-web"] = inventory.MaxObjectRefs
		ingress.Objects = nil
		for i := range inventory.MaxObjectRefs {
			ingress.Objects = append(ingress.Objects, inventory.ObjectRef{Namespace: "a-web", Name: fmt.Sprintf("w%d", i)})
		}
		ingress.ObjectsOmitted = 2
	}
	inv.APIUsage = []inventory.APIUsage{ingress}
	return inv
}

// The documented exception to a scoped gate's byte-for-byte answer (#72):
// the collector lists at most inventory.MaxObjectRefs objects per API
// across the cluster, in namespace order, so which of payments' objects
// its share lists depends on how many objects at that API sort before
// them. Where web's hundred left payments' two unlisted, the finding
// lists only the PR's object and counts payments' as omitted, and its
// detail says neither "manifest" nor "stored" of all three; everything
// else, the verdict, status, score, title, count, namespaces and teams,
// is the same as on a cluster of payments' two alone, and nothing of
// web's is named.
func TestScopedGateObjectListingFollowsTheCollectorsCap(t *testing.T) {
	_, st, ts, _ := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	pushScopeCluster(t, ts, "cap-web", capInventory(true))
	pushScopeCluster(t, ts, "cap-none", capInventory(false))
	const pr = `apiVersion: extensions/v1beta1
kind: Ingress
metadata: {name: pr, namespace: pay-prod}
`
	type finding struct {
		Key, Source, Severity, Title, Detail string
		Teams, Namespaces                    []string
		Objects                              []inventory.ObjectRef
		ObjectsOmitted                       int
	}
	type answer struct {
		Status, Header string
		Verdict        string    `json:"verdict"`
		ClusterVerdict string    `json:"clusterVerdict"`
		Score          int       `json:"score"`
		Findings       []finding `json:"findings"`
	}
	read := func(cluster string) answer {
		resp, raw := postGate(t, ts, "?target=1.35&fail-on=never&cluster="+cluster, "pay-tok", pr, "application/x-yaml")
		var a answer
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		if len(a.Findings) != 1 {
			t.Fatalf("%s: want the Ingress finding alone: %s", cluster, raw)
		}
		for _, s := range []string{"a-web", `"web"`, `"w0"`, "102"} {
			if bytes.Contains(raw, []byte(s)) {
				t.Errorf("%s: payments-scoped gate names %s: %s", cluster, s, raw)
			}
		}
		a.Status, a.Header = resp.Status, resp.Header.Get("X-Upgradescope-Verdict")
		return a
	}
	web, none := read("cap-web"), read("cap-none")

	ref := func(name string) inventory.ObjectRef { return inventory.ObjectRef{Namespace: "pay-prod", Name: name} }
	prRef := ref("pr")
	prRef.Line = 1
	for _, c := range []struct {
		got     finding
		objects []inventory.ObjectRef
		omitted int
		detail  string
	}{
		{web.Findings[0], []inventory.ObjectRef{prRef}, 2, "3 object(s) use this API: pay-prod (3)."},
		{none.Findings[0], []inventory.ObjectRef{ref("p0"), ref("p1"), prRef}, 0, "3 object(s) still stored/served at this version: pay-prod (3)."},
	} {
		if !reflect.DeepEqual(c.got.Objects, c.objects) || c.got.ObjectsOmitted != c.omitted || c.got.Detail != c.detail {
			t.Errorf("finding lists %+v (+%d), detail %q; want %+v (+%d), %q", c.got.Objects, c.got.ObjectsOmitted, c.got.Detail, c.objects, c.omitted, c.detail)
		}
	}
	// What the cap does not reach.
	web.Findings[0].Objects, web.Findings[0].ObjectsOmitted, web.Findings[0].Detail = nil, 0, ""
	none.Findings[0].Objects, none.Findings[0].ObjectsOmitted, none.Findings[0].Detail = nil, 0, ""
	if !reflect.DeepEqual(web, none) {
		t.Errorf("the cap reaches past the finding's object list and detail:\n%+v\n%+v", web, none)
	}
}

// The cap reaches a scoped gate's score too, as the docs say: payments'
// Helm release stores its two Ingresses at extensions/v1beta1. Where the
// share lists them live, the release's manifest is left to the live
// finding; where web's hundred left them unlisted, the engine cannot
// match them, and the release gets a blocker of its own, which lowers
// the share's score. What the PR introduces, and so the verdict, its
// header and the status, is the same on both.
func TestScopedGateScoreFollowsTheCollectorsCap(t *testing.T) {
	_, st, ts, _ := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	got := map[bool]gateSummary{}
	for _, web := range []bool{true, false} {
		inv := capInventory(web)
		inv.HelmReleases = []inventory.HelmRelease{{
			Name: "pay", Namespace: "pay-prod", ChartName: "pay", ChartVersion: "1.0.0", Status: "deployed", Revision: 1,
			ManifestAPIs: []inventory.APIUsage{{
				Group: "extensions", Version: "v1beta1", Kind: "Ingress", Count: 2, Namespaces: map[string]int{"": 2},
				Objects: []inventory.ObjectRef{{Name: "p0", Line: 3}, {Name: "p1", Line: 9}},
			}},
		}}
		name := fmt.Sprintf("helm-%v", web)
		pushScopeCluster(t, ts, name, inv)
		resp, raw := postGate(t, ts, "?target=1.35&cluster="+name, "pay-tok",
			"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: pr, namespace: pay-prod}\n", "application/x-yaml")
		got[web] = summarize(t, resp, raw)
	}
	web, none := got[true], got[false]
	if web.Status != none.Status || web.Header != none.Header || web.Verdict != none.Verdict || web.ClusterVerdict != none.ClusterVerdict {
		t.Errorf("the cap reaches the verdict or status:\n%+v\n%+v", web, none)
	}
	const helm = "removed-api/helm-release/pay-prod/pay"
	has := func(s gateSummary) bool {
		for _, f := range s.Findings {
			if f.Key == helm {
				return true
			}
		}
		return false
	}
	if !has(web) || has(none) || web.Score >= none.Score {
		t.Errorf("want %s, and a lower score, only behind web's hundred (the documented cap dependence):\n%+v\n%+v", helm, web, none)
	}
}
