package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// #68: a cluster moving from v0.1.x or an rc has accepted findings on
// many objects under the pre-v0.2.0 ignore keys, and some annotations may
// lack a reason. status.notAssessed keeps its first 32 entries, so those
// warnings must not push the cluster's real capability gap out of it: the
// gap stays listed (and named by Ready), the deprecation is one line, and
// the per-object reason-less warnings come after the gaps.
func TestTickLegacyAnnotationsKeepCapabilityGaps(t *testing.T) {
	inv := eolNginxInventory()
	inv.Capabilities[inventory.CapHelm] = inventory.CapabilityStatus{Reason: "secrets list forbidden"}
	use := inventory.APIUsage{Group: "extensions", Version: "v1beta1", Kind: "Ingress"}
	for i := range 40 {
		use.Objects = append(use.Objects, inventory.ObjectRef{Namespace: "shop", Name: fmt.Sprintf("accepted-%02d", i),
			Ignore: "removed-api", IgnoreReason: "replaced by HTTPRoute", IgnoreLegacyKey: true})
		use.Objects = append(use.Objects, inventory.ObjectRef{Namespace: "shop", Name: fmt.Sprintf("no-reason-%02d", i),
			Ignore: "removed-api", IgnoreLegacyKey: true})
	}
	use.Count = len(use.Objects)
	inv.APIUsage = []inventory.APIUsage{use}

	dyn := fakeDyn()
	r := testRunner(t, dyn, "")
	r.collectFn = func(context.Context) inventory.Inventory { return inv }
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if s := r.last.reports[0].Suppressed; len(s) != 1 || len(s[0].Objects) != 40 {
		t.Fatalf("suppressed = %+v, want the 40 objects with a reason accepted", s)
	}

	st := readCRStatus(t, dyn, crd.DefaultName)
	if !slices.Contains(st.NotAssessed, "helm: secrets list forbidden") {
		t.Errorf("notAssessed lost the capability gap:\n%s", strings.Join(st.NotAssessed, "\n"))
	}
	var deprecations int
	for _, n := range st.NotAssessed {
		if strings.Contains(n, "are deprecated and read only until v0.3.0") {
			deprecations++
		}
	}
	if deprecations != 1 {
		t.Errorf("notAssessed has %d deprecation lines, want 1:\n%s", deprecations, strings.Join(st.NotAssessed, "\n"))
	}
	i := slices.IndexFunc(st.Conditions, func(c metav1.Condition) bool { return c.Type == crd.ConditionReady })
	if i < 0 || !strings.Contains(st.Conditions[i].Message, "helm") {
		t.Errorf("conditions = %+v, want Ready to name the helm gap", st.Conditions)
	}
	requireCRValidAgainstCRD(t, dyn, crd.DefaultName)
}
