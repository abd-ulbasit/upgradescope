package collect

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #48 acceptance: a cert-manager CRD that no longer serves v1alpha2, in
// the scanned files with a cert-manager.io/v1alpha2 Certificate, is a
// blocker naming the Certificate's file and line. A custom resource whose
// CRD is not in the files (the ClusterIssuer) is not assessed, not a
// finding.
func TestCollectFilesCRDVersions(t *testing.T) {
	k := loadKB(t)
	inv, _, err := CollectFiles("testdata/crds", k)
	if err != nil {
		t.Fatal(err)
	}
	want := []inventory.CRD{{
		Group: "cert-manager.io", Kind: "Certificate", Plural: "certificates",
		Versions: []inventory.CRDVersion{
			{Name: "v1alpha2"}, {Name: "v1alpha3"}, {Name: "v1beta1"},
			{Name: "v1", Served: true, Storage: true},
		},
		Usage: []inventory.APIUsage{{
			Group: "cert-manager.io", Version: "v1alpha2", Kind: "Certificate", Count: 1,
			Namespaces: map[string]int{"shop": 1},
			Objects:    []inventory.ObjectRef{{Namespace: "shop", Name: "web-tls", File: "cert-manager/certificates.yaml", Line: 1}},
		}},
	}}
	if !reflect.DeepEqual(inv.CRDs, want) {
		t.Errorf("CRDs =\n%+v\nwant\n%+v", inv.CRDs, want)
	}
	wantCap := inventory.CapabilityStatus{Available: true, Partial: true,
		Reason:  "no CRD in the scanned files for cert-manager.io/v1alpha2 ClusterIssuer, so whether its version is served was not assessed",
		Skipped: []string{"cert-manager.io/v1alpha2 ClusterIssuer"}}
	if got := inv.Capabilities[inventory.CapCRDs]; !reflect.DeepEqual(got, wantCap) {
		t.Errorf("crds = %+v\nwant   %+v", got, wantCap)
	}

	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 35}, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	var crdFindings []engine.Finding
	for _, f := range rep.Findings {
		if f.Category == engine.CatCRDVersion {
			crdFindings = append(crdFindings, f)
		}
	}
	if len(crdFindings) != 1 {
		t.Fatalf("crd-version findings = %+v, want one", crdFindings)
	}
	f := crdFindings[0]
	if f.Key != "crd-version/unserved/cert-manager.io/v1alpha2/Certificate" || f.Severity != engine.SevBlocker {
		t.Errorf("finding %s (%s), want the unserved blocker", f.Key, f.Severity)
	}
	if !strings.Contains(f.Detail, "1 manifest object(s) use this version") || len(f.Objects) != 1 || f.Objects[0].File != "cert-manager/certificates.yaml" {
		t.Errorf("finding = %+v", f)
	}
	if rep.Verdict != engine.VerdictBlocked {
		t.Errorf("verdict = %s, want blocked", rep.Verdict)
	}
}

// Files mode reads CRDs wherever kubectl reads objects (a List, JSON) and
// judges custom resources at a deprecated served version; one at a served
// version is not usage. Nothing is skipped when every custom resource has
// its CRD: built-in objects and kustomization files need none.
func TestCollectManifestsCRDVersions(t *testing.T) {
	stream := `apiVersion: v1
kind: List
items:
  - apiVersion: apiextensions.k8s.io/v1
    kind: CustomResourceDefinition
    metadata: {name: httproutes.gateway.networking.k8s.io}
    spec:
      group: gateway.networking.k8s.io
      names: {kind: HTTPRoute, plural: httproutes}
      scope: Namespaced
      versions:
        - {name: v1beta1, served: true, storage: false, deprecated: true, deprecationWarning: "use v1"}
        - {name: v1, served: true, storage: true}
---
{"apiVersion": "gateway.networking.k8s.io/v1beta1", "kind": "HTTPRoute", "metadata": {"name": "web", "namespace": "shop"}}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: api, namespace: shop}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, namespace: shop}
---
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [routes.yaml]
`
	inv, err := CollectManifests(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := inv.Capabilities[inventory.CapCRDs]; !got.Available || got.Partial {
		t.Errorf("crds = %+v, want available", got)
	}
	want := []inventory.CRD{{
		Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Plural: "httproutes",
		Versions: []inventory.CRDVersion{
			{Name: "v1beta1", Served: true, Deprecated: true, DeprecationWarning: "use v1"},
			{Name: "v1", Served: true, Storage: true},
		},
		Usage: []inventory.APIUsage{{
			Group: "gateway.networking.k8s.io", Version: "v1beta1", Kind: "HTTPRoute", Count: 1,
			Namespaces: map[string]int{"shop": 1},
			Objects:    []inventory.ObjectRef{{Namespace: "shop", Name: "web", Line: 15}},
		}},
	}}
	if !reflect.DeepEqual(inv.CRDs, want) {
		t.Errorf("CRDs =\n%+v\nwant\n%+v", inv.CRDs, want)
	}
}
