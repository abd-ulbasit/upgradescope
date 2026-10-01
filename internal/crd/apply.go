// Package crd owns the ClusterReadiness custom resource: its embedded CRD
// manifest, plain JSON-tagged Go types (no codegen), and apply/status logic
// via the dynamic client.
package crd

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsv1typed "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

// FieldManager is the server-side apply field manager EnsureCRD writes the
// CRD spec under. Fields other managers own (GitOps labels and annotations,
// kubectl's last-applied annotation) are left alone.
const FieldManager = "upgradescope-agent"

// ErrCRDNotInstalled means the ClusterReadiness CRD is absent and the agent
// may not create it. The Helm chart grants no CRD create: its crds/
// directory installs the CRD, and the agent only keeps the schema current.
var ErrCRDNotInstalled = errors.New("ClusterReadiness CRD is not installed")

// EnsureCRD keeps the ClusterReadiness CRD in step with the embedded
// manifest. An in-sync CRD gets no write at all. A drifted one is fixed with
// a forced server-side apply of the manifest under FieldManager, so the spec
// matches this binary while labels and annotations owned by others survive.
// A missing CRD is created (and waited on until Established) when the
// caller may create CRDs, as with an admin kubeconfig; under the chart's
// RBAC the create is forbidden and the error wraps ErrCRDNotInstalled.
func EnsureCRD(ctx context.Context, apiext apiextensionsclient.Interface) error {
	var want apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(Manifest, &want); err != nil {
		return fmt.Errorf("parse embedded CRD manifest: %w", err)
	}
	crds := apiext.ApiextensionsV1().CustomResourceDefinitions()
	existing, err := crds.Get(ctx, want.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return createCRD(ctx, crds, &want)
	}
	if err != nil {
		return fmt.Errorf("get ClusterReadiness CRD: %w", err)
	}

	// Compare against the manifest as the apiserver stores it (defaulted),
	// or every restart would see spurious drift.
	defaulted := want.DeepCopy()
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(defaulted)
	if equality.Semantic.DeepEqual(existing.Spec, defaulted.Spec) {
		return nil
	}
	body, err := yaml.YAMLToJSON(Manifest)
	if err != nil {
		return fmt.Errorf("convert embedded CRD manifest: %w", err)
	}
	// Force: the schema must match this binary even if another manager
	// last touched those spec fields.
	force := true
	_, err = crds.Patch(ctx, want.Name, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	if err != nil {
		return fmt.Errorf("apply ClusterReadiness CRD: %w", err)
	}
	return nil
}

// createCRD installs a missing CRD and waits for it to be Established.
func createCRD(ctx context.Context, crds apiextensionsv1typed.CustomResourceDefinitionInterface, want *apiextensionsv1.CustomResourceDefinition) error {
	_, err := crds.Create(ctx, want, metav1.CreateOptions{FieldManager: FieldManager})
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("%w and the agent may not create it: install it from the Helm chart's crds/ "+
			"(helm install, or kubectl apply -f deploy/chart/crds/): %w", ErrCRDNotInstalled, err)
	}
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ClusterReadiness CRD: %w", err)
	}
	return waitEstablished(ctx, crds, want.Name)
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

// EnsureObject creates the ClusterReadiness CR if absent, with spec.targets
// set to targets (empty spec when there are none). It never overwrites an
// existing object; SetTargets does that.
func EnsureObject(ctx context.Context, dyn dynamic.Interface, name string, targets []string) error {
	spec := map[string]interface{}{}
	if len(targets) > 0 {
		spec["targets"] = stringsToInterfaces(targets)
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": Group + "/" + Version,
		"kind":       Kind,
		"metadata":   map[string]interface{}{"name": name},
		"spec":       spec,
	}}
	_, err := dyn.Resource(GVR()).Create(ctx, obj, metav1.CreateOptions{})
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}
	if apierrors.IsNotFound(err) {
		// A create only 404s when the resource is not served at all.
		return fmt.Errorf("create clusterreadiness %q: %w; install it with the chart's crds/ "+
			"(helm install, or kubectl apply -f deploy/chart/crds/): %w", name, ErrCRDNotInstalled, err)
	}
	return fmt.Errorf("create clusterreadiness %q: %w", name, err)
}

// SetTargets replaces spec.targets of an existing ClusterReadiness with a
// merge patch, leaving the rest of the object alone.
func SetTargets(ctx context.Context, dyn dynamic.Interface, name string, targets []string) error {
	body, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"targets": targets},
	})
	if err != nil {
		return fmt.Errorf("encode targets patch: %w", err)
	}
	if _, err := dyn.Resource(GVR()).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("set clusterreadiness %q spec.targets: %w", name, err)
	}
	return nil
}

// stringsToInterfaces converts for unstructured content, which only holds
// []interface{} lists.
func stringsToInterfaces(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
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
