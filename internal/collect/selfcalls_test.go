package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// selfFeedingClients is a fake cluster at serverVersion whose /metrics
// behaves like apiserver_requested_deprecated_apis: once any client LISTs
// a resource in deprecated ("group/version resource" → removed_release),
// its row is there on every later scrape (until an apiserver restart,
// which these fakes never do).
func selfFeedingClients(t *testing.T, serverVersion string, deprecated map[string]string, lists ...*metav1.APIResourceList) Clients {
	t.Helper()
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-self")}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: serverVersion}}},
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.Resources = lists
	disc.FakedServerVersion = &version.Info{GitVersion: serverVersion}
	meta := metaClient()
	metrics := func() string {
		var rows []string
		for _, gvr := range listedGVRs(meta) {
			removed, ok := deprecated[gvr.GroupVersion().String()+" "+gvr.Resource]
			if !ok {
				continue
			}
			row := fmt.Sprintf(`apiserver_requested_deprecated_apis{group=%q,removed_release=%q,resource=%q,subresource="",version=%q} 1`,
				gvr.Group, removed, gvr.Resource, gvr.Version)
			if !slices.Contains(rows, row) {
				rows = append(rows, row)
			}
		}
		return "# TYPE apiserver_requested_deprecated_apis gauge\n" + strings.Join(rows, "\n") + "\n"
	}
	return Clients{Kube: cs, Metadata: meta, Discovery: disc, RESTClient: metricsRESTClientFunc(t, metrics)}
}

// Issue #123 acceptance: Collect and Evaluate three times against one
// unchanged cluster whose /metrics records the scanner's own deprecated
// LISTs. Every report must be byte-identical, and none may name the
// scanner as a client still requesting a deprecated API.
func TestCollect_SelfFeedingDeprecatedCalls(t *testing.T) {
	list := metav1.Verbs{"get", "list"}
	r := func(name, kind string) metav1.APIResource {
		return metav1.APIResource{Name: name, Kind: kind, Verbs: list}
	}
	core := resources("v1", r("pods", "Pod"), r("endpoints", "Endpoints"), r("componentstatuses", "ComponentStatus"))
	deprecated := map[string]string{ // every deprecated endpoint either cluster serves
		"policy/v1beta1 podsecuritypolicies":          "1.25",
		"policy/v1beta1 poddisruptionbudgets":         "1.25",
		"coordination.k8s.io/v1beta1 leasecandidates": "1.39",
		"v1 componentstatuses":                        "",
		"v1 endpoints":                                "",
	}
	cases := []struct {
		name    string
		server  string
		lists   []*metav1.APIResourceList
		targets []string
		// verdict and score at the first target, when set
		verdict engine.Verdict
		score   int
	}{
		// 1.24 with PodSecurityPolicy served but unused: the scanner must
		// LIST policy/v1beta1 (nothing else serves PSP), and that request
		// used to turn scan 2 onwards into blocked/75.
		{"1.24 with no PodSecurityPolicies", "v1.24.17", []*metav1.APIResourceList{core,
			resources("policy/v1", r("poddisruptionbudgets", "PodDisruptionBudget")),
			resources("policy/v1beta1", r("poddisruptionbudgets", "PodDisruptionBudget"), r("podsecuritypolicies", "PodSecurityPolicy")),
		}, []string{"1.25"}, engine.VerdictReady, 100},
		// 1.37 with coordination.k8s.io/v1beta1 enabled: LeaseCandidate is
		// served only there and removed in 1.39.
		{"1.37 with only v1beta1 LeaseCandidates", "v1.37.0", []*metav1.APIResourceList{core,
			resources("coordination.k8s.io/v1", r("leases", "Lease")),
			resources("coordination.k8s.io/v1beta1", r("leasecandidates", "LeaseCandidate")),
		}, []string{"1.38", "1.39"}, "", 0},
	}
	k := loadKB(t)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := selfFeedingClients(t, tc.server, deprecated, tc.lists...)
			var first []byte
			for run := 1; run <= 3; run++ {
				inv := Collect(context.Background(), c, k, Options{})
				var reports []engine.Report
				for _, raw := range tc.targets {
					target, err := inventory.ParseVersion(raw)
					if err != nil {
						t.Fatal(err)
					}
					rep := engine.Evaluate(inv, k, target, now)
					for _, f := range rep.Findings {
						if f.Category == engine.CatDeprecatedAPIInUse || f.Severity == engine.SevBlocker {
							t.Errorf("run %d, target %s: %s finding %s: %s", run, raw, f.Severity, f.Key, f.Detail)
						}
					}
					reports = append(reports, rep)
				}
				if tc.verdict != "" && (reports[0].Verdict != tc.verdict || reports[0].Score != tc.score) {
					t.Errorf("run %d: %s/%d, want %s/%d (gaps %+v)", run, reports[0].Verdict, reports[0].Score, tc.verdict, tc.score, reports[0].NotAssessed)
				}
				got, err := json.Marshal(reports)
				if err != nil {
					t.Fatal(err)
				}
				if first == nil {
					first = got
				} else if !bytes.Equal(got, first) {
					t.Errorf("run %d report differs from run 1:\n%s\nrun 1:\n%s", run, got, first)
				}
			}
			for _, gvr := range listedGVRs(c.Metadata.(*metadatafake.FakeMetadataClient)) {
				if gvr.Resource == "componentstatuses" || gvr.Resource == "endpoints" {
					t.Errorf("listed %v: deprecated, never removed", gvr)
				}
			}
		})
	}
}

// failingDiscovery is discovery that does not get through this scan: with
// fail nil it fails outright (a stall past the api-usage step's deadline),
// otherwise it leaves out the group/versions in fail, as a broken
// aggregated API or a group that timed out does (ErrGroupDiscoveryFailed).
type failingDiscovery struct {
	*discoveryfake.FakeDiscovery
	fail map[schema.GroupVersion]bool
}

func (d failingDiscovery) ServerGroupsAndResourcesWithContext(ctx context.Context) ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	if d.fail == nil {
		return nil, nil, context.DeadlineExceeded
	}
	groups, lists, err := d.FakeDiscovery.ServerGroupsAndResourcesWithContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	gde := &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{}}
	lists = slices.DeleteFunc(lists, func(l *metav1.APIResourceList) bool {
		gv, _ := schema.ParseGroupVersion(l.GroupVersion)
		if d.fail[gv] {
			gde.Groups[gv] = errors.New("the server is currently unable to handle the request")
		}
		return d.fail[gv]
	})
	return groups, lists, gde
}

// #239: the scanner's own deprecated LIST of an earlier scan stays in
// apiserver_requested_deprecated_apis until the apiserver restarts. When a
// later scan's discovery does not get through (it stalls, or leaves out the
// group), api-usage cannot say what the scanner lists there, and that row
// used to be scored as another client's: a deprecated-api-in-use blocker
// for the scanner's own traffic. It is withheld instead, and named.
func TestSelfListedRowsAreNotAttributedWhenDiscoveryFails(t *testing.T) {
	list := metav1.Verbs{"get", "list"}
	r := func(name, kind string) metav1.APIResource {
		return metav1.APIResource{Name: name, Kind: kind, Verbs: list}
	}
	deprecated := map[string]string{"policy/v1beta1 podsecuritypolicies": "1.25"}
	lists := []*metav1.APIResourceList{
		resources("v1", r("pods", "Pod")),
		resources("policy/v1", r("poddisruptionbudgets", "PodDisruptionBudget")),
		resources("policy/v1beta1", r("poddisruptionbudgets", "PodDisruptionBudget"), r("podsecuritypolicies", "PodSecurityPolicy")),
	}
	cases := []struct {
		name string
		fail map[schema.GroupVersion]bool
		// the api-usage capability: unavailable, or partial naming PSP
		apiUsageAvailable bool
	}{
		{"discovery fails outright", nil, false},
		{"discovery leaves out policy/v1beta1", map[schema.GroupVersion]bool{{Group: "policy", Version: "v1beta1"}: true}, true},
	}
	k := loadKB(t)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	target := inventory.Version{Major: 1, Minor: 25}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := selfFeedingClients(t, "v1.24.17", deprecated, lists...)
			Collect(context.Background(), c, k, Options{}) // lists PSPs at policy/v1beta1, which the metric now shows

			c.Discovery = failingDiscovery{c.Discovery.(*discoveryfake.FakeDiscovery), tc.fail}
			inv := Collect(context.Background(), c, k, Options{})
			if !slices.ContainsFunc(inv.DeprecatedCalls, func(d inventory.DeprecatedCall) bool { return d.Resource == "podsecuritypolicies" }) {
				t.Fatalf("deprecated calls %+v: the fake must serve the earlier scan's PSP row", inv.DeprecatedCalls)
			}
			au := inv.Capabilities[inventory.CapAPIUsage]
			if au.Available != tc.apiUsageAvailable || (au.Available && !slices.Contains(au.Skipped, "policy/v1beta1 PodSecurityPolicy")) {
				t.Errorf("api-usage %+v: want available %v, and PSP unchecked when available", au, tc.apiUsageAvailable)
			}
			dc := inv.Capabilities[inventory.CapDeprecatedCalls]
			if !dc.Available || !dc.Partial || !slices.Contains(dc.Skipped, "policy/v1beta1 podsecuritypolicies") {
				t.Errorf("deprecated-calls %+v: want partial, skipping the row it cannot attribute", dc)
			}
			if !strings.Contains(dc.Reason, "policy/v1beta1") {
				t.Errorf("deprecated-calls reason %q: must say which group/version it could not attribute", dc.Reason)
			}
			rep := engine.Evaluate(inv, k, target, now)
			for _, f := range rep.Findings {
				if f.Category == engine.CatDeprecatedAPIInUse {
					t.Errorf("finding %s: %s; the row may be the scanner's own", f.Key, f.Detail)
				}
			}
			if rep.Verdict == engine.VerdictBlocked {
				t.Errorf("verdict blocked (gaps %+v)", rep.NotAssessed)
			}
			if !slices.ContainsFunc(rep.NotAssessed, func(g engine.CapabilityGap) bool { return g.Capability == inventory.CapAPIUsage }) {
				t.Errorf("not assessed %+v: api-usage must stay a gap", rep.NotAssessed)
			}
		})
	}
}
