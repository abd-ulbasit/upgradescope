package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Issue #199 (API-01): objects of a flagged kind that nothing can be
// attributed to are one info finding per API, key suffixed so it never
// collides with the API's deprecated-api finding, and move neither verdict
// nor score, even for a kind already removed at the target.
func TestEvalAuthorshipUnknown(t *testing.T) {
	inv := inventory.Inventory{
		Source:        inventory.SourceCluster,
		ServerVersion: "v1.33.4",
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapAPIUsage: {Available: true}, inventory.CapVersions: {Available: true},
		},
		APIAuthorshipUnknown: []inventory.APIUsage{
			{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 2,
				Namespaces: map[string]int{"shop": 2},
				Objects:    []inventory.ObjectRef{{Namespace: "shop", Name: "b"}, {Namespace: "shop", Name: "a"}}},
			// Not flagged by the KB, whatever the inventory says.
			{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress", Count: 1, Namespaces: map[string]int{"shop": 1}},
			{Group: "cert-manager.io", Version: "v1", Kind: "Certificate", Count: 1, Namespaces: map[string]int{"shop": 1}},
		},
		Namespaces: testNamespaces(),
	}
	k := testKB()

	fs := evalAuthorshipUnknown(inv, k, nil)
	want := []Finding{{
		Category: CatDeprecatedAPI, Severity: SevInfo,
		Key:   "deprecated-api/networking.k8s.io/v1beta1/Ingress/authorship-unknown",
		Title: "networking.k8s.io/v1beta1 Ingress: authorship unknown (2 objects)",
		Detail: "2 object(s) have no managedFields entry to attribute them by (none outside the status subresource and the control plane) and no usable last-applied annotation, " +
			"which is what creating one with an empty spec leaves, so who writes them, and through which API version, cannot be told. " +
			"They are stored the same through every served version of Ingress: not counted as use of networking.k8s.io/v1beta1, no effect on the verdict or score. Namespaces: shop (2).",
		Teams:      []string{"storefront"},
		Namespaces: []string{"shop"},
		Citations:  []string{deprecationGuideURL},
		Objects:    []inventory.ObjectRef{{Namespace: "shop", Name: "a"}, {Namespace: "shop", Name: "b"}},
	}}
	if !reflect.DeepEqual(fs, want) {
		t.Errorf("findings = %+v\nwant       %+v", fs, want)
	}

	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	target := inventory.Version{Major: 1, Minor: 34} // networking.k8s.io/v1beta1 Ingress went in 1.22
	with := Evaluate(inv, k, target, now)
	inv.APIAuthorshipUnknown = nil
	without := Evaluate(inv, k, target, now)
	if with.Verdict != without.Verdict || with.Score != without.Score {
		t.Errorf("verdict/score %s/%d with authorship-unknown objects, %s/%d without; want the same", with.Verdict, with.Score, without.Verdict, without.Score)
	}
	if len(with.Findings) != len(without.Findings)+1 {
		t.Errorf("findings = %+v, want exactly the authorship-unknown one more than %+v", with.Findings, without.Findings)
	}
}
