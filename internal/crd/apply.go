// Package crd owns the ClusterReadiness custom resource: its embedded CRD
// manifest, plain JSON-tagged Go types (no codegen), and apply/status logic
// via the dynamic client.
package crd

import (
	"context"
	_ "embed"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

// Manifest is the embedded ClusterReadiness CRD manifest. It is the single
// source of truth: EnsureCRD converges the cluster to it, and the Helm chart
// ships a copy in crds/.
//
//go:embed manifest.yaml
var Manifest []byte

// EnsureCRD installs or updates the ClusterReadiness CRD from the embedded
// manifest. Create-or-update semantics: existing CRDs are overwritten with
// the manifest's spec (conflicts retried). Callers may treat failure as
// non-fatal when the CRD is pre-installed out of band (e.g. Helm crds/).
func EnsureCRD(ctx context.Context, apiext apiextensionsclient.Interface) error {
	var want apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(Manifest, &want); err != nil {
		return fmt.Errorf("parse embedded CRD manifest: %w", err)
	}
	crds := apiext.ApiextensionsV1().CustomResourceDefinitions()
	_, err := crds.Create(ctx, &want, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ClusterReadiness CRD: %w", err)
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, gerr := crds.Get(ctx, want.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		updated := want.DeepCopy()
		updated.ResourceVersion = existing.ResourceVersion
		_, uerr := crds.Update(ctx, updated, metav1.UpdateOptions{})
		return uerr
	})
	if err != nil {
		return fmt.Errorf("update ClusterReadiness CRD: %w", err)
	}
	return nil
}
