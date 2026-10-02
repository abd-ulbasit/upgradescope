package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// eolNginxInventory is a fully assessed cluster whose only problem is an
// EOL ingress-nginx.
func eolNginxInventory() inventory.Inventory {
	caps := map[inventory.Capability]inventory.CapabilityStatus{}
	for _, c := range []inventory.Capability{inventory.CapAPIUsage, inventory.CapDeprecatedCalls, inventory.CapHelm, inventory.CapAddOns, inventory.CapVersions} {
		caps[c] = inventory.CapabilityStatus{Available: true}
	}
	return inventory.Inventory{
		SchemaVersion: 1, ClusterID: "c", Source: inventory.SourceCluster,
		ServerVersion: "v1.35.2", Capabilities: caps,
		AddOns: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.0", Namespaces: []string{"ingress-nginx"}, Source: "image"}},
		Nodes:  []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.35.2"}},
	}
}

// spec.ignore is applied every tick: the accepted EOL add-on leaves the
// status ready with one suppressed finding, and an expired rule is
// reported in notAssessed instead of applied.
func TestTickAppliesSpecIgnore(t *testing.T) {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.Group + "/" + crd.Version,
		"kind":       crd.Kind,
		"metadata":   map[string]interface{}{"name": crd.DefaultName},
		"spec": map[string]interface{}{
			"targets": []interface{}{"1.36", "1.37"},
			"ignore": []interface{}{
				map[string]interface{}{"key": "eol-addon/ingress-nginx", "reason": "migrating to Gateway API"},
				map[string]interface{}{"category": "kb-stale", "reason": "r", "expires": "2020-01-01"},
			},
		},
	}}
	dyn := fakeDyn(cr)
	r := testRunner(t, dyn, "")
	r.collectFn = func(context.Context) inventory.Inventory { return eolNginxInventory() }

	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	st := readCRStatus(t, dyn, crd.DefaultName)
	for _, ts := range st.Targets {
		if ts.Blockers != 0 || ts.Suppressed != 1 || ts.Verdict != "ready" {
			t.Errorf("target %s = %+v, want ready with 1 suppressed", ts.Target, ts)
		}
	}
	var expired []string
	for _, n := range st.NotAssessed {
		if strings.Contains(n, "expired on 2020-01-01") {
			expired = append(expired, n)
		}
	}
	if len(expired) != 1 || !strings.HasPrefix(expired[0], "spec.ignore: ignore[1]") {
		t.Errorf("notAssessed = %v, want the expired rule named once", st.NotAssessed)
	}
	requireCRValidAgainstCRD(t, dyn, crd.DefaultName)

	// Without the rule the same cluster is blocked.
	dyn = fakeDyn()
	r = testRunner(t, dyn, "")
	r.collectFn = func(context.Context) inventory.Inventory { return eolNginxInventory() }
	if err := r.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := readCRStatus(t, dyn, crd.DefaultName); !slices.ContainsFunc(st.Targets, func(ts crd.TargetStatus) bool { return ts.Blockers > 0 }) {
		t.Errorf("targets = %+v, want the EOL add-on blocking without spec.ignore", st.Targets)
	}
}
