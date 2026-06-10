// Package crd owns the ClusterReadiness custom resource: its embedded CRD
// manifest, plain JSON-tagged Go types (no codegen), and apply/status logic
// via the dynamic client.
package crd

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsv1typed "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

// Manifest is the embedded ClusterReadiness CRD manifest. It is the single
// source of truth: EnsureCRD converges the cluster to it, and the Helm chart
// ships a copy in crds/.
//
//go:embed manifest.yaml
var Manifest []byte

// Establishment polling knobs (vars so tests can shrink the timeout).
var (
	establishPollInterval = 100 * time.Millisecond
	establishTimeout      = 10 * time.Second
)

// isEstablished reports whether the apiserver serves the CRD's endpoints
// (condition Established=True).
func isEstablished(c *apiextensionsv1.CustomResourceDefinition) bool {
	for _, cond := range c.Status.Conditions {
		if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
			return true
		}
	}
	return false
}

// waitEstablished polls until the named CRD reports Established=True. Without
// this, a freshly created CRD's CR endpoint 404s the first tick and the next
// attempt is a full interval away.
func waitEstablished(ctx context.Context, crds apiextensionsv1typed.CustomResourceDefinitionInterface, name string) error {
	err := wait.PollUntilContextTimeout(ctx, establishPollInterval, establishTimeout, true,
		func(ctx context.Context) (bool, error) {
			got, gerr := crds.Get(ctx, name, metav1.GetOptions{})
			if gerr != nil {
				return false, nil // transient; keep polling until timeout
			}
			return isEstablished(got), nil
		})
	if err != nil {
		return fmt.Errorf("ClusterReadiness CRD created but not Established within %s: %w", establishTimeout, err)
	}
	return nil
}

// EnsureCRD installs or updates the ClusterReadiness CRD from the embedded
// manifest. Create-or-update semantics: existing CRDs are overwritten with
// the manifest's spec (conflicts retried). After a fresh create it waits for
// the Established condition so the first tick can write status immediately.
// Callers may treat failure as non-fatal when the CRD is pre-installed out
// of band (e.g. Helm crds/).
func EnsureCRD(ctx context.Context, apiext apiextensionsclient.Interface) error {
	var want apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(Manifest, &want); err != nil {
		return fmt.Errorf("parse embedded CRD manifest: %w", err)
	}
	crds := apiext.ApiextensionsV1().CustomResourceDefinitions()
	_, err := crds.Create(ctx, &want, metav1.CreateOptions{})
	if err == nil {
		return waitEstablished(ctx, crds, want.Name)
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

// ReadSpec returns the ClusterReadiness spec. found=false (with nil error)
// means the object does not exist; callers typically EnsureObject then.
func ReadSpec(ctx context.Context, dyn dynamic.Interface, name string) (Spec, bool, error) {
	obj, err := dyn.Resource(GVR()).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Spec{}, false, nil
	}
	if err != nil {
		return Spec{}, false, fmt.Errorf("get clusterreadiness %q: %w", name, err)
	}
	raw, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return Spec{}, true, fmt.Errorf("read spec of clusterreadiness %q: %w", name, err)
	}
	if !found {
		return Spec{}, true, nil
	}
	var s Spec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &s); err != nil {
		return Spec{}, true, fmt.Errorf("decode spec of clusterreadiness %q: %w", name, err)
	}
	return s, true, nil
}

// EnsureObject creates the ClusterReadiness CR with an empty spec if absent.
// It never overwrites an existing object (user-set spec.targets survive).
func EnsureObject(ctx context.Context, dyn dynamic.Interface, name string) error {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": Group + "/" + Version,
		"kind":       Kind,
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{},
	}}
	_, err := dyn.Resource(GVR()).Create(ctx, obj, metav1.CreateOptions{})
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("create clusterreadiness %q: %w", name, err)
}

// WriteStatus replaces the status subresource, retrying on conflict with a
// fresh read each attempt.
func WriteStatus(ctx context.Context, dyn dynamic.Interface, name string, st Status) error {
	stMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&st)
	if err != nil {
		return fmt.Errorf("convert status: %w", err)
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, gerr := dyn.Resource(GVR()).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		obj.Object["status"] = stMap
		_, uerr := dyn.Resource(GVR()).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
		return uerr
	})
	if err != nil {
		return fmt.Errorf("update clusterreadiness %q status: %w", name, err)
	}
	return nil
}
