package crd

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const crdName = "clusterreadinesses.upgradescope.dev"

func parseManifest(t *testing.T) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	var c apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(Manifest, &c); err != nil {
		t.Fatalf("embedded manifest does not parse as a v1 CRD: %v", err)
	}
	return c
}

func TestManifestShape(t *testing.T) {
	c := parseManifest(t)
	if c.Name != crdName {
		t.Errorf("name = %q, want %q", c.Name, crdName)
	}
	if c.Spec.Group != "upgradescope.dev" {
		t.Errorf("group = %q, want upgradescope.dev", c.Spec.Group)
	}
	if c.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Errorf("scope = %q, want Cluster", c.Spec.Scope)
	}
	if c.Spec.Names.Plural != "clusterreadinesses" || c.Spec.Names.Kind != "ClusterReadiness" {
		t.Errorf("names = %+v, want plural clusterreadinesses kind ClusterReadiness", c.Spec.Names)
	}
	if len(c.Spec.Versions) != 1 || c.Spec.Versions[0].Name != "v1alpha1" {
		t.Fatalf("versions = %+v, want exactly v1alpha1", c.Spec.Versions)
	}
	v := c.Spec.Versions[0]
	if !v.Served || !v.Storage {
		t.Error("v1alpha1 must be served and storage")
	}
	if v.Subresources == nil || v.Subresources.Status == nil {
		t.Error("status subresource not enabled")
	}
	cols := map[string]bool{}
	for _, pc := range v.AdditionalPrinterColumns {
		cols[pc.Name] = true
	}
	for _, want := range []string{"Target", "Score", "Ready", "Age"} {
		if !cols[want] {
			t.Errorf("printer column %q missing (have %v)", want, cols)
		}
	}
	if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
		t.Fatal("openAPIV3Schema missing")
	}
	if _, ok := v.Schema.OpenAPIV3Schema.Properties["status"]; !ok {
		t.Error("schema has no status property")
	}
}

func TestEnsureCRDCreates(t *testing.T) {
	ctx := context.Background()
	fc := apiextfake.NewSimpleClientset()
	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("EnsureCRD: %v", err)
	}
	got, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("CRD not created: %v", err)
	}
	if got.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Errorf("created CRD scope = %q, want Cluster", got.Spec.Scope)
	}
}

func TestEnsureCRDUpdatesExisting(t *testing.T) {
	ctx := context.Background()
	fc := apiextfake.NewSimpleClientset()
	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("first EnsureCRD: %v", err)
	}
	// Simulate drift: wipe printer columns on the stored object.
	got, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got.Spec.Versions[0].AdditionalPrinterColumns = nil
	if _, err := fc.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Second EnsureCRD must take the AlreadyExists → update path and reconcile.
	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("second EnsureCRD: %v", err)
	}
	got2, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Spec.Versions[0].AdditionalPrinterColumns) == 0 {
		t.Error("EnsureCRD did not restore printer columns on the update path")
	}
}
