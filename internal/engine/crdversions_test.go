package engine

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// certManagerCRD is cert-manager's Certificate CRD part-way through its
// v1 migration: v1alpha2 deprecated, v1alpha3 no longer served, v1 the
// storage version.
func certManagerCRD() inventory.CRD {
	return inventory.CRD{
		Group: "cert-manager.io", Kind: "Certificate", Plural: "certificates",
		Versions: []inventory.CRDVersion{
			{Name: "v1alpha2", Served: true, Deprecated: true, DeprecationWarning: "cert-manager.io/v1alpha2 Certificate is deprecated; use cert-manager.io/v1"},
			{Name: "v1alpha3", Served: false},
			{Name: "v1", Served: true, Storage: true},
		},
		StoredVersions: []string{"v1alpha3", "v1"},
	}
}

func findingByKey(fs []Finding, key string) (Finding, bool) {
	for _, f := range fs {
		if f.Key == key {
			return f, true
		}
	}
	return Finding{}, false
}

// A served version marked deprecated: true is a warning when custom
// resources are written through it, carrying the CRD's deprecationWarning
// and the writers; with none found it is info.
func TestEvalCRDVersionsDeprecatedInUse(t *testing.T) {
	crd := certManagerCRD()
	crd.Usage = []inventory.APIUsage{{
		Group: "cert-manager.io", Version: "v1alpha2", Kind: "Certificate",
		Count: 2, Namespaces: map[string]int{"shop": 1, "default": 1},
		Objects: []inventory.ObjectRef{
			{Namespace: "shop", Name: "web-tls", Manager: "argocd-controller"},
			{Namespace: "default", Name: "api-tls", Manager: "kubectl-client-side-apply"},
		},
	}}
	inv := clusterInv()
	inv.Namespaces = testNamespaces()
	inv.CRDs = []inventory.CRD{crd}
	got := evalCRDVersions(inv, inventory.Version{Major: 1, Minor: 35}, nil)

	f, ok := findingByKey(got, "crd-version/deprecated/cert-manager.io/v1alpha2/Certificate")
	if !ok {
		t.Fatalf("no deprecated finding in %+v", got)
	}
	if f.Category != CatCRDVersion || f.Severity != SevWarning {
		t.Errorf("category/severity = %s/%s, want crd-version/warning", f.Category, f.Severity)
	}
	if want := "cert-manager.io/v1alpha2 Certificate is a deprecated CRD version (2 objects)"; f.Title != want {
		t.Errorf("Title = %q, want %q", f.Title, want)
	}
	for _, want := range []string{
		"2 object(s) written through this version: default (1), shop (1).",
		"Written by: argocd-controller, kubectl-client-side-apply.",
		`The CRD says: "cert-manager.io/v1alpha2 Certificate is deprecated; use cert-manager.io/v1".`,
		"independent of Kubernetes 1.35",
	} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("Detail lacks %q:\n%s", want, f.Detail)
		}
	}
	if want := "move these objects to cert-manager.io/v1 before an add-on upgrade stops serving v1alpha2"; f.Remediation != want {
		t.Errorf("Remediation = %q, want %q", f.Remediation, want)
	}
	if !reflect.DeepEqual(f.Teams, []string{"core", "storefront"}) || !reflect.DeepEqual(f.Namespaces, []string{"default", "shop"}) {
		t.Errorf("teams %v namespaces %v", f.Teams, f.Namespaces)
	}
	if len(f.Objects) != 2 || f.Objects[0].Name != "api-tls" {
		t.Errorf("objects not copied and sorted: %+v", f.Objects)
	}

	// No usage: the deprecation alone is info.
	inv.CRDs = []inventory.CRD{certManagerCRD()}
	got = evalCRDVersions(inv, inventory.Version{Major: 1, Minor: 35}, nil)
	f, ok = findingByKey(got, "crd-version/deprecated/cert-manager.io/v1alpha2/Certificate")
	if !ok || f.Severity != SevInfo {
		t.Fatalf("unused deprecated version: %+v (found %v), want an info finding", f, ok)
	}
	if want := "cert-manager.io/v1alpha2 Certificate is a deprecated CRD version"; f.Title != want {
		t.Errorf("Title = %q, want %q", f.Title, want)
	}
	if !strings.Contains(f.Detail, "No custom resource was found written through this version.") ||
		!strings.Contains(f.Detail, "cert-manager.io/v1alpha2 Certificate is deprecated; use cert-manager.io/v1") {
		t.Errorf("Detail = %q", f.Detail)
	}
}

// Custom resources at a version the CRD does not serve, whether marked
// served: false or dropped from spec.versions, are rejected: a blocker.
func TestEvalCRDVersionsUnservedInUse(t *testing.T) {
	crd := certManagerCRD()
	crd.Usage = []inventory.APIUsage{
		{Group: "cert-manager.io", Version: "v1alpha3", Kind: "Certificate", Count: 1,
			Namespaces: map[string]int{"shop": 1},
			Objects:    []inventory.ObjectRef{{Namespace: "shop", Name: "web-tls", Manager: "flux"}}},
		{Group: "cert-manager.io", Version: "v1beta1", Kind: "Certificate", Count: 1,
			Namespaces: map[string]int{"": 1},
			Objects:    []inventory.ObjectRef{{Name: "edge", File: "certs.yaml", Line: 3}}},
	}
	inv := clusterInv()
	inv.CRDs = []inventory.CRD{crd}
	got := evalCRDVersions(inv, inventory.Version{Major: 1, Minor: 35}, nil)

	f, ok := findingByKey(got, "crd-version/unserved/cert-manager.io/v1alpha3/Certificate")
	if !ok || f.Severity != SevBlocker || f.Category != CatCRDVersion {
		t.Fatalf("served: false version in use: %+v (found %v), want a crd-version blocker", f, ok)
	}
	if want := "cert-manager.io/v1alpha3 Certificate is not served by its CRD (1 object)"; f.Title != want {
		t.Errorf("Title = %q, want %q", f.Title, want)
	}
	for _, want := range []string{"CRD certificates.cert-manager.io marks v1alpha3 served: false", "it serves v1alpha2, v1", "Written by: flux."} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("Detail lacks %q:\n%s", want, f.Detail)
		}
	}
	// Live objects are judged by managedFields, so a manager that stopped
	// writing (or is gone) leaves an entry that keeps them listed.
	if want := "move these objects to cert-manager.io/v1; the apiserver rejects writes at v1alpha3. " +
		"A named manager that no longer writes them keeps them listed through its managedFields entry " +
		"until another manager takes over its fields or the entry is removed"; f.Remediation != want {
		t.Errorf("Remediation = %q, want %q", f.Remediation, want)
	}

	f, ok = findingByKey(got, "crd-version/unserved/cert-manager.io/v1beta1/Certificate")
	if !ok || f.Severity != SevBlocker {
		t.Fatalf("version missing from the CRD: %+v (found %v), want a blocker", f, ok)
	}
	for _, want := range []string{"1 manifest object(s) use this version: namespace unset (1).", "CRD certificates.cert-manager.io does not list v1beta1"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("Detail lacks %q:\n%s", want, f.Detail)
		}
	}
	if want := "move these objects to cert-manager.io/v1; the apiserver rejects writes at v1beta1"; f.Remediation != want {
		t.Errorf("manifest Remediation = %q, want %q (no manager note)", f.Remediation, want)
	}
}

// A status.storedVersions entry the CRD no longer serves blocks the next
// CRD update that drops it: a warning with the migration as remediation.
// The storage version and served entries are fine.
func TestEvalCRDVersionsStoredUnserved(t *testing.T) {
	inv := clusterInv()
	inv.CRDs = []inventory.CRD{certManagerCRD()}
	got := evalCRDVersions(inv, inventory.Version{Major: 1, Minor: 35}, nil)

	f, ok := findingByKey(got, "crd-version/stored-unserved/cert-manager.io/v1alpha3/Certificate")
	if !ok || f.Severity != SevWarning {
		t.Fatalf("stored unserved version: %+v (found %v), want a warning", f, ok)
	}
	if want := "CRD certificates.cert-manager.io lists unserved version v1alpha3 in status.storedVersions"; f.Title != want {
		t.Errorf("Title = %q, want %q", f.Title, want)
	}
	for _, want := range []string{"kube-storage-version-migrator", "v1", "status.storedVersions"} {
		if !strings.Contains(f.Remediation, want) {
			t.Errorf("Remediation lacks %q: %s", want, f.Remediation)
		}
	}
	for _, f := range got {
		if strings.HasPrefix(f.Key, "crd-version/stored-unserved/") && f.Key != "crd-version/stored-unserved/cert-manager.io/v1alpha3/Certificate" {
			t.Errorf("unexpected stored-version finding %s", f.Key)
		}
	}
}

// A CRD with nothing deprecated, unserved or stale produces nothing.
func TestEvalCRDVersionsCleanCRD(t *testing.T) {
	inv := clusterInv()
	inv.CRDs = []inventory.CRD{{
		Group: "example.com", Kind: "Widget", Plural: "widgets",
		Versions:       []inventory.CRDVersion{{Name: "v1beta1", Served: true}, {Name: "v1", Served: true, Storage: true}},
		StoredVersions: []string{"v1beta1", "v1"},
	}}
	if got := evalCRDVersions(inv, inventory.Version{Major: 1, Minor: 35}, nil); len(got) != 0 {
		t.Errorf("findings for a clean CRD: %+v", got)
	}
}

// When the collector could not check a deprecated version's objects, the
// info finding says so instead of claiming none were found.
func TestEvalCRDVersionsUncheckedUsage(t *testing.T) {
	inv := clusterInv()
	inv.Capabilities[inventory.CapCRDs] = inventory.CapabilityStatus{Available: true, Partial: true,
		Reason: "list cert-manager.io/v1 certificates: forbidden", Skipped: []string{"cert-manager.io/v1alpha2 Certificate"}}
	inv.CRDs = []inventory.CRD{certManagerCRD()}
	got := evalCRDVersions(inv, inventory.Version{Major: 1, Minor: 35}, nil)
	f, ok := findingByKey(got, "crd-version/deprecated/cert-manager.io/v1alpha2/Certificate")
	if !ok || f.Severity != SevInfo {
		t.Fatalf("got %+v (found %v)", f, ok)
	}
	if !strings.Contains(f.Detail, "Custom resources written through this version were not checked") {
		t.Errorf("Detail = %q", f.Detail)
	}
}

// An inventory from a collector that predates CRD checks does not report
// the crds capability: the cluster's CRDs were not assessed, which is a
// gap, though not a required one (CRD versions are the add-ons', not the
// Kubernetes target's). The same holds for a files inventory an older CLI
// saved and `scan --inventory` re-evaluates.
func TestEvaluateCRDsGap(t *testing.T) {
	inv := clusterInv()
	delete(inv.Capabilities, inventory.CapCRDs)
	r := Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 35}, testNow)
	predates := CapabilityGap{Capability: inventory.CapCRDs,
		Reason: "not reported by the collector, which predates CRD checks; upgrade it to assess CRD versions"}
	if want := []CapabilityGap{predates}; !reflect.DeepEqual(r.NotAssessed, want) {
		t.Errorf("NotAssessed = %+v, want %+v", r.NotAssessed, want)
	}
	if r.Verdict != VerdictReady {
		t.Errorf("Verdict = %s, want ready: the crds gap is not required", r.Verdict)
	}

	files := filesInv()
	delete(files.Capabilities, inventory.CapCRDs)
	files.APIUsage = []inventory.APIUsage{{Group: "cert-manager.io", Version: "v1alpha2", Kind: "Certificate", Count: 1}}
	r = Evaluate(files, testKB(), inventory.Version{Major: 1, Minor: 35}, testNow)
	if !slices.ContainsFunc(r.NotAssessed, func(g CapabilityGap) bool { return reflect.DeepEqual(g, predates) }) {
		t.Errorf("files inventory without crds: NotAssessed = %+v, want it to include %+v", r.NotAssessed, predates)
	}

	// A blocker from a CRD version is a blocker like any other.
	inv = clusterInv()
	crd := certManagerCRD()
	crd.Usage = []inventory.APIUsage{{Group: "cert-manager.io", Version: "v1alpha3", Kind: "Certificate", Count: 1}}
	inv.CRDs = []inventory.CRD{crd}
	if r := Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 35}, testNow); r.Verdict != VerdictBlocked {
		t.Errorf("Verdict = %s, want blocked", r.Verdict)
	}
}

// A custom resource row the gate listed only manifests of, while it
// counts the cluster's objects too, is worded as both
// (TestEvalAPIUsageManifestsInAClusterRow).
func TestCRDUsageManifestsInAClusterRow(t *testing.T) {
	u := inventory.APIUsage{
		Group: "cert-manager.io", Version: "v1beta1", Kind: "Certificate", Count: 3, Namespaces: map[string]int{"pay-prod": 3},
		Objects: []inventory.ObjectRef{{Namespace: "pay-prod", Name: "pr", Line: 1}}, ObjectsOmitted: 2,
	}
	var f Finding
	if got, want := crdUsage(&f, u, clusterInv()), "3 object(s) use this version: pay-prod (3)."; got != want {
		t.Errorf("crdUsage = %q, want %q", got, want)
	}
	files := clusterInv()
	files.Source = inventory.SourceFiles
	if got, want := crdUsage(&f, u, files), "3 manifest object(s) use this version: pay-prod (3)."; got != want {
		t.Errorf("files-mode crdUsage = %q, want %q", got, want)
	}
}
