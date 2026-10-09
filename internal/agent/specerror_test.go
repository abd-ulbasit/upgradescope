package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

// specCR is the ClusterReadiness at generation 7 with the given spec.
func specCR(spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.Group + "/" + crd.Version,
		"kind":       crd.Kind,
		"metadata":   map[string]interface{}{"name": crd.DefaultName, "generation": int64(7)},
		"spec":       spec,
	}}
}

// requireNoStatusWritten fails when the tick wrote a status, and checks
// the CR is marked as not current instead.
func requireNoStatusWritten(t *testing.T, dyn *dynamicfake.FakeDynamicClient, why string) {
	t.Helper()
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "status" {
			t.Errorf("%s: the tick wrote a status (%v), want none for targets it could not know", why, a)
		}
	}
	obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), crd.DefaultName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
		t.Errorf("%s: status = %v, want none", why, obj.Object["status"])
	}
	if v, ok := obj.GetAnnotations()[crd.StatusErrorAnnotation]; !ok || v == "" {
		t.Errorf("%s: no %s annotation, want the CR marked", why, crd.StatusErrorAnnotation)
	}
}

// #238: a spec read that fails must not evaluate the default target and
// stamp the current generation as observed (a false "current" for Argo CD
// and kubectl wait). The tick fails, the CR is marked, and the next tick
// that reads the spec writes the status and clears the mark.
func TestTickFailedSpecReadWritesNoStatus(t *testing.T) {
	dyn := fakeDyn(specCR(map[string]interface{}{"targets": []interface{}{"1.38"}})).(*dynamicfake.FakeDynamicClient)
	gets := 0
	dyn.PrependReactor("get", crd.Plural, func(k8stesting.Action) (bool, runtime.Object, error) {
		if gets++; gets == 1 {
			return true, nil, apierrors.NewTimeoutError("etcdserver: request timed out", 1)
		}
		return false, nil, nil
	})
	r := testRunner(t, dyn, "")
	if err := r.tick(context.Background()); err == nil || !strings.Contains(err.Error(), "request timed out") {
		t.Fatalf("tick err = %v, want the spec read's", err)
	}
	if len(r.last.reports) != 0 {
		t.Errorf("reports = %+v, want none evaluated without a spec", r.last.reports)
	}
	requireNoStatusWritten(t, dyn, "failed spec read")

	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("next tick: %v", err)
	}
	st := readCRStatus(t, dyn, crd.DefaultName)
	if st.ObservedGeneration != 7 || len(st.Targets) != 1 || st.Targets[0].Target != "1.38" {
		t.Errorf("status observedGeneration=%d targets=%+v, want 7 and the spec's 1.38", st.ObservedGeneration, st.Targets)
	}
	if v, ok := statusErrorAnnotation(t, dyn); ok {
		t.Errorf("annotation %q survived the next good tick", v)
	}
}

// A spec that is read but cannot be decoded carries the real generation:
// evaluating defaults under it would claim, every tick, a spec the agent
// never evaluated.
func TestTickUndecodableSpecWritesNoStatus(t *testing.T) {
	dyn := fakeDyn(specCR(map[string]interface{}{"targets": "1.38"})).(*dynamicfake.FakeDynamicClient)
	r := testRunner(t, dyn, "")
	if err := r.tick(context.Background()); err == nil || !strings.Contains(err.Error(), "decode spec") {
		t.Fatalf("tick err = %v, want the decode error", err)
	}
	requireNoStatusWritten(t, dyn, "undecodable spec")
}

// With --targets the flags are what is evaluated, but a spec.targets patch
// that fails leaves a spec that still lists other targets at the current
// generation: a status for the flags' targets stamped with it would claim
// a spec it did not evaluate.
func TestTickFailedSpecTargetsPatchWritesNoStatus(t *testing.T) {
	dyn := fakeDyn(specCR(map[string]interface{}{"targets": []interface{}{"1.35"}})).(*dynamicfake.FakeDynamicClient)
	dyn.PrependReactor("patch", crd.Plural, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(k8stesting.PatchAction).GetPatch()), `"spec"`) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: crd.Group, Resource: crd.Plural}, crd.DefaultName, errors.New("RBAC: cannot patch"))
		}
		return false, nil, nil // the marker's annotation patch is allowed
	})
	r := targetsRunner(t, dyn, "1.37")
	if err := r.tick(context.Background()); err == nil || !strings.Contains(err.Error(), "spec.targets") {
		t.Fatalf("tick err = %v, want the spec.targets patch error", err)
	}
	requireNoStatusWritten(t, dyn, "failed spec.targets patch")
}
