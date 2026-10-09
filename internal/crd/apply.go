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
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsv1typed "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
// attempt is a full interval away. The wait ends at establishTimeout, or
// earlier at ctx's deadline (an agent tick can give its CRD check less),
// so the error says how long it waited.
func waitEstablished(ctx context.Context, crds apiextensionsv1typed.CustomResourceDefinitionInterface, name string) error {
	start := time.Now()
	err := wait.PollUntilContextTimeout(ctx, establishPollInterval, establishTimeout, true,
		func(ctx context.Context) (bool, error) {
			got, gerr := crds.Get(ctx, name, metav1.GetOptions{})
			if gerr != nil {
				return false, nil // transient; keep polling until timeout
			}
			return isEstablished(got), nil
		})
	if err != nil {
		return fmt.Errorf("ClusterReadiness CRD created but not Established after %s: %w",
			time.Since(start).Round(10*time.Millisecond), err)
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

// ReadSpec returns the ClusterReadiness spec and the metadata.generation it
// was read at, for WriteStatus to stamp as observed. found=false (with nil
// error) means the object does not exist; callers typically EnsureObject
// then.
func ReadSpec(ctx context.Context, dyn dynamic.Interface, name string) (spec Spec, generation int64, found bool, err error) {
	spec, generation, obj, err := ReadSpecObject(ctx, dyn, name)
	return spec, generation, obj != nil, err
}

// ReadSpecObject is ReadSpec returning the object read as well (nil when
// it does not exist, with a nil error), for WriteStatusOver to write the
// status over without reading it again.
func ReadSpecObject(ctx context.Context, dyn dynamic.Interface, name string) (spec Spec, generation int64, obj *unstructured.Unstructured, err error) {
	obj, err = dyn.Resource(GVR()).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Spec{}, 0, nil, nil
	}
	if err != nil {
		return Spec{}, 0, nil, fmt.Errorf("get clusterreadiness %q: %w", name, err)
	}
	spec, generation, err = specOf(obj, name)
	return spec, generation, obj, err
}

// specOf decodes obj's spec.
func specOf(obj *unstructured.Unstructured, name string) (Spec, int64, error) {
	gen := obj.GetGeneration()
	raw, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return Spec{}, gen, fmt.Errorf("read spec of clusterreadiness %q: %w", name, err)
	}
	if !found {
		return Spec{}, gen, nil
	}
	var s Spec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &s); err != nil {
		return Spec{}, gen, fmt.Errorf("decode spec of clusterreadiness %q: %w", name, err)
	}
	return s, gen, nil
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
// merge patch, leaving the rest of the object alone. It returns the patched
// object's generation.
func SetTargets(ctx context.Context, dyn dynamic.Interface, name string, targets []string) (int64, error) {
	body, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"targets": targets},
	})
	if err != nil {
		return 0, fmt.Errorf("encode targets patch: %w", err)
	}
	obj, err := dyn.Resource(GVR()).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return 0, fmt.Errorf("set clusterreadiness %q spec.targets: %w", name, err)
	}
	return obj.GetGeneration(), nil
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

// ErrStatusErrorNotCleared is what WriteStatus returns, wrapped, when the
// status was written but the StatusErrorAnnotation of an earlier failure
// could not be removed: the status is current, only the marker outlived it.
var ErrStatusErrorNotCleared = errors.New("status written, marker not cleared")

// WriteStatus replaces the status subresource, retrying on conflict with a
// fresh read each attempt. st.ObservedGeneration should be the generation
// whose spec was evaluated (from ReadSpec or SetTargets), so a spec edited
// since is not claimed as observed; zero stamps the generation of the
// object being written. It sets the Ready condition from st, keeping the
// stored condition's lastTransitionTime while its status is unchanged.
// Whatever st holds, notAssessed is bounded here (boundNotAssessed), the
// one place every source of notes passes.
func WriteStatus(ctx context.Context, dyn dynamic.Interface, name string, st Status) error {
	return WriteStatusOver(ctx, dyn, name, st, nil)
}

// WriteStatusOver is WriteStatus whose first attempt writes over current,
// the object as the caller read it moments before (ReadSpecObject), instead
// of reading it again (#228): the update carries current's resourceVersion,
// so an object changed since is a conflict, and the retry reads it fresh.
// A nil current reads it first, as WriteStatus does.
func WriteStatusOver(ctx context.Context, dyn dynamic.Interface, name string, st Status, current *unstructured.Unstructured) error {
	st.NotAssessed = boundNotAssessed(st.NotAssessed)
	marked := false
	if current != nil {
		current = current.DeepCopy()
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := current
		current = nil // a retry reads it again
		if obj == nil {
			var gerr error
			if obj, gerr = dyn.Resource(GVR()).Get(ctx, name, metav1.GetOptions{}); gerr != nil {
				return gerr
			}
		}
		_, marked = obj.GetAnnotations()[StatusErrorAnnotation]
		out := st
		if out.ObservedGeneration == 0 {
			out.ObservedGeneration = obj.GetGeneration()
		}
		out.Conditions = storedConditions(obj)
		ready := ReadyCondition(st)
		ready.ObservedGeneration = out.ObservedGeneration
		meta.SetStatusCondition(&out.Conditions, ready)
		stMap, cerr := runtime.DefaultUnstructuredConverter.ToUnstructured(&out)
		if cerr != nil {
			return fmt.Errorf("convert status: %w", cerr)
		}
		obj.Object["status"] = stMap
		_, uerr := dyn.Resource(GVR()).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
		return uerr
	})
	if err != nil {
		return fmt.Errorf("update clusterreadiness %q status: %w", name, err)
	}
	if marked {
		// The status is current again: drop the marker of an earlier
		// failure. A failure here is retried by the next tick's write.
		if err := patchStatusError(ctx, dyn, name, nil); err != nil {
			return fmt.Errorf("clear clusterreadiness %q %s annotation: %w: %w", name, StatusErrorAnnotation, ErrStatusErrorNotCleared, err)
		}
	}
	return nil
}

// maxStatusErrorReason bounds the reason in a StatusErrorAnnotation value:
// an apiserver's error can carry a whole request.
const maxStatusErrorReason = 240

// MarkStatusError annotates the ClusterReadiness with when its status write
// failed and why (StatusErrorAnnotation), by a merge patch of the object
// itself: an agent that lost only the status subresource can still say so.
// A role that lost patch on the object too cannot, and gets the error; its
// /readyz and metrics are then the only signals. cause is cut to one short
// line; at is the failure's time.
func MarkStatusError(ctx context.Context, dyn dynamic.Interface, name string, cause error, at time.Time) error {
	reason := strings.Join(strings.Fields(cause.Error()), " ")
	if r := []rune(reason); len(r) > maxStatusErrorReason {
		reason = string(r[:maxStatusErrorReason]) + "…"
	}
	value := at.UTC().Format(time.RFC3339) + " " + reason
	if err := patchStatusError(ctx, dyn, name, &value); err != nil {
		return fmt.Errorf("mark clusterreadiness %q with %s: %w", name, StatusErrorAnnotation, err)
	}
	return nil
}

// patchStatusError sets the annotation to *value, or removes it for nil.
func patchStatusError(ctx context.Context, dyn dynamic.Interface, name string, value *string) error {
	body, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": map[string]interface{}{StatusErrorAnnotation: value}},
	})
	if err != nil {
		return fmt.Errorf("encode annotation patch: %w", err)
	}
	_, err = dyn.Resource(GVR()).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

// storedConditions returns the object's current status.conditions. An
// absent or undecodable status yields none: the write then starts the
// list afresh.
func storedConditions(obj *unstructured.Unstructured) []metav1.Condition {
	raw, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		return nil
	}
	var st Status
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &st); err != nil {
		return nil
	}
	return st.Conditions
}
