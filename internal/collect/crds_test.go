package collect

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

const certWarning = "cert-manager.io/v1alpha2 Certificate is deprecated in v1.4+, unavailable in v1.6+; use cert-manager.io/v1 Certificate"

// crdObject builds a CustomResourceDefinition as the apiserver returns it.
func crdObject(group, kind, plural string, stored []string, versions ...apiextensionsv1.CustomResourceDefinitionVersion) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group:    group,
			Names:    apiextensionsv1.CustomResourceDefinitionNames{Kind: kind, Plural: plural},
			Scope:    apiextensionsv1.NamespaceScoped,
			Versions: versions,
		},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{StoredVersions: stored},
	}
}

// certificatesCRD is cert-manager's Certificate CRD mid-migration:
// v1alpha2 deprecated, v1alpha3 no longer served but still stored, v1 the
// storage version.
func certificatesCRD() *apiextensionsv1.CustomResourceDefinition {
	warning := certWarning
	return crdObject("cert-manager.io", "Certificate", "certificates", []string{"v1alpha3", "v1"},
		apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1alpha2", Served: true, Deprecated: true, DeprecationWarning: &warning},
		apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1alpha3", Served: false},
		apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: true, Storage: true},
	)
}

// issuersCRD has nothing deprecated or unserved: its custom resources are
// never listed.
func issuersCRD() *apiextensionsv1.CustomResourceDefinition {
	return crdObject("cert-manager.io", "Issuer", "issuers", []string{"v1"},
		apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: true, Storage: true})
}

// certificates are stored Certificates, served at v1: web-tls applied via
// the deprecated v1alpha2, legacy-tls via the unserved v1alpha3, api-tls
// via v1 only.
func certificates() []runtime.Object {
	return servedAt("Certificate", []string{"cert-manager.io/v1"},
		obj{namespace: "shop", name: "web-tls", managed: []metav1.ManagedFieldsEntry{appliedAt("argocd-controller", "cert-manager.io/v1alpha2", 1)}},
		obj{namespace: "shop", name: "legacy-tls", managed: []metav1.ManagedFieldsEntry{appliedAt("flux", "cert-manager.io/v1alpha3", 1)}},
		obj{namespace: "default", name: "api-tls", managed: []metav1.ManagedFieldsEntry{appliedAt("helm", "cert-manager.io/v1", 1)}},
	)
}

func TestCollectCRDs(t *testing.T) {
	ext := apiextensionsfake.NewClientset(issuersCRD(), certificatesCRD())
	meta := metaClient(certificates())
	var inv inventory.Inventory
	if err := collectCRDs(context.Background(), ext, meta, &inv); err != nil {
		t.Fatalf("collectCRDs: %v", err)
	}
	want := []inventory.CRD{
		{
			Group: "cert-manager.io", Kind: "Certificate", Plural: "certificates",
			Versions: []inventory.CRDVersion{
				{Name: "v1alpha2", Served: true, Deprecated: true, DeprecationWarning: certWarning},
				{Name: "v1alpha3"},
				{Name: "v1", Served: true, Storage: true},
			},
			StoredVersions: []string{"v1alpha3", "v1"},
			Usage: []inventory.APIUsage{
				{Group: "cert-manager.io", Version: "v1alpha2", Kind: "Certificate", Count: 1, Namespaces: map[string]int{"shop": 1},
					Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "web-tls", Manager: "argocd-controller"}}},
				{Group: "cert-manager.io", Version: "v1alpha3", Kind: "Certificate", Count: 1, Namespaces: map[string]int{"shop": 1},
					Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "legacy-tls", Manager: "flux"}}},
			},
		},
		{
			Group: "cert-manager.io", Kind: "Issuer", Plural: "issuers",
			Versions:       []inventory.CRDVersion{{Name: "v1", Served: true, Storage: true}},
			StoredVersions: []string{"v1"},
		},
	}
	if !reflect.DeepEqual(inv.CRDs, want) {
		t.Errorf("CRDs =\n%+v\nwant\n%+v", inv.CRDs, want)
	}
	// Custom resources are listed once, at a served version that is not
	// deprecated (listing v1alpha2 would make the scanner a deprecated
	// caller), and only for CRDs with a version to look for.
	var listed []string
	for _, a := range meta.Actions() {
		if a.GetVerb() == "list" {
			listed = append(listed, a.GetResource().String())
		}
	}
	if want := []string{"cert-manager.io/v1, Resource=certificates"}; !reflect.DeepEqual(listed, want) {
		t.Errorf("listed %q, want %q", listed, want)
	}
}

// A forbidden custom-resource LIST leaves the CRDs themselves read: the
// capability is partial, naming the versions whose use went unchecked.
func TestCollectCRDsForbiddenCustomResources(t *testing.T) {
	ext := apiextensionsfake.NewClientset(certificatesCRD())
	meta := metaClient(certificates())
	meta.PrependReactor("list", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "cert-manager.io", Resource: "certificates"}, "", errors.New("RBAC"))
	})
	var inv inventory.Inventory
	err := collectCRDs(context.Background(), ext, meta, &inv)
	var pe partialError
	if !errors.As(err, &pe) || !pe.incomplete {
		t.Fatalf("err = %#v, want an incomplete partialError", err)
	}
	if want := []string{"cert-manager.io/v1alpha2 Certificate", "cert-manager.io/v1alpha3 Certificate"}; !reflect.DeepEqual(pe.skipped, want) {
		t.Errorf("skipped = %q, want %q", pe.skipped, want)
	}
	if !strings.Contains(pe.msg, "list cert-manager.io/v1 certificates") {
		t.Errorf("reason = %q, want it to name the LIST", pe.msg)
	}
	if len(inv.CRDs) != 1 || len(inv.CRDs[0].StoredVersions) != 2 || inv.CRDs[0].Usage != nil {
		t.Errorf("CRDs = %+v, want the CRD read without usage", inv.CRDs)
	}
}

// A CRD whose every served version is deprecated cannot be listed without
// a deprecated request: its custom resources are left unchecked.
func TestCollectCRDsEveryServedVersionDeprecated(t *testing.T) {
	ext := apiextensionsfake.NewClientset(crdObject("example.com", "Widget", "widgets", []string{"v1beta1"},
		apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1beta1", Served: true, Storage: true, Deprecated: true}))
	meta := metaClient()
	var inv inventory.Inventory
	err := collectCRDs(context.Background(), ext, meta, &inv)
	var pe partialError
	if !errors.As(err, &pe) || !reflect.DeepEqual(pe.skipped, []string{"example.com/v1beta1 Widget"}) {
		t.Fatalf("err = %#v, want a partialError skipping example.com/v1beta1 Widget", err)
	}
	if len(meta.Actions()) != 0 {
		t.Errorf("listed custom resources: %v", meta.Actions())
	}
}

// Without CRDs readable, nothing about them is known: the capability
// degrades.
func TestCollectCRDsForbiddenCRDs(t *testing.T) {
	ext := apiextensionsfake.NewClientset()
	ext.PrependReactor("list", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, "", errors.New("RBAC"))
	})
	var inv inventory.Inventory
	err := collectCRDs(context.Background(), ext, metaClient(), &inv)
	var pe partialError
	if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "customresourcedefinitions") {
		t.Errorf("err = %v, want a plain error naming customresourcedefinitions", err)
	}
}

// #48 acceptance, end to end through Collect and the engine: a deprecated
// served version with custom resources written through it is a warning
// quoting the CRD's deprecationWarning; a status.storedVersions entry the
// CRD no longer serves is a warning with the migration as remediation.
func TestCollectCRDsEndToEnd(t *testing.T) {
	c := Clients{APIExtensions: apiextensionsfake.NewClientset(certificatesCRD()), Metadata: metaClient(certificates())}
	k := loadKB(t)
	inv := Collect(context.Background(), c, k, Options{})
	if st := inv.Capabilities[inventory.CapCRDs]; !st.Available || st.Partial {
		t.Fatalf("crds = %+v, want available", st)
	}
	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 35}, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	got := map[string]engine.Finding{}
	for _, f := range rep.Findings {
		got[f.Key] = f
	}
	dep := got["crd-version/deprecated/cert-manager.io/v1alpha2/Certificate"]
	if dep.Severity != engine.SevWarning || !strings.Contains(dep.Detail, certWarning) {
		t.Errorf("deprecated finding = %+v, want a warning quoting the deprecationWarning", dep)
	}
	stored := got["crd-version/stored-unserved/cert-manager.io/v1alpha3/Certificate"]
	if stored.Severity != engine.SevWarning || !strings.Contains(stored.Remediation, "kube-storage-version-migrator") {
		t.Errorf("stored-version finding = %+v, want a warning with migration remediation", stored)
	}
	if f := got["crd-version/unserved/cert-manager.io/v1alpha3/Certificate"]; f.Severity != engine.SevBlocker {
		t.Errorf("unserved finding = %+v, want a blocker", f)
	}
}
