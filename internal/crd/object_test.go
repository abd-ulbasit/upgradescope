package crd

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newDynFake(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVR(): Kind + "List"},
		objects...,
	)
}

func newCRObject(name string, targets ...string) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	if len(targets) > 0 {
		list := make([]interface{}, len(targets))
		for i, s := range targets {
			list[i] = s
		}
		spec["targets"] = list
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": Group + "/" + Version,
		"kind":       Kind,
		"metadata":   map[string]interface{}{"name": name},
		"spec":       spec,
	}}
}

func TestReadSpecNotFound(t *testing.T) {
	dyn := newDynFake()
	spec, found, err := ReadSpec(context.Background(), dyn, DefaultName)
	if err != nil {
		t.Fatalf("ReadSpec on absent object: %v", err)
	}
	if found {
		t.Error("found = true for absent object")
	}
	if len(spec.Targets) != 0 {
		t.Errorf("spec = %+v, want zero value", spec)
	}
}

func TestReadSpecTargets(t *testing.T) {
	dyn := newDynFake(newCRObject(DefaultName, "1.36", "1.37"))
	spec, found, err := ReadSpec(context.Background(), dyn, DefaultName)
	if err != nil || !found {
		t.Fatalf("ReadSpec: found=%v err=%v", found, err)
	}
	if want := []string{"1.36", "1.37"}; !reflect.DeepEqual(spec.Targets, want) {
		t.Errorf("Targets = %v, want %v", spec.Targets, want)
	}
}

func TestEnsureObjectCreatesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dyn := newDynFake()
	if err := EnsureObject(ctx, dyn, DefaultName); err != nil {
		t.Fatalf("EnsureObject: %v", err)
	}
	obj, err := dyn.Resource(GVR()).Get(ctx, DefaultName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("object not created: %v", err)
	}
	if obj.GetKind() != Kind {
		t.Errorf("kind = %q, want %q", obj.GetKind(), Kind)
	}
	// Second call must not error on AlreadyExists and must not clobber spec.
	if _, err := dyn.Resource(GVR()).Update(ctx, newCRObject(DefaultName, "1.37"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureObject(ctx, dyn, DefaultName); err != nil {
		t.Fatalf("second EnsureObject: %v", err)
	}
	spec, _, err := ReadSpec(ctx, dyn, DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.37"}; !reflect.DeepEqual(spec.Targets, want) {
		t.Errorf("EnsureObject clobbered spec: %v, want %v", spec.Targets, want)
	}
}

func TestWriteStatus(t *testing.T) {
	ctx := context.Background()
	dyn := newDynFake(newCRObject(DefaultName))
	st := Status{
		ObservedServerVersion: "v1.35.2",
		KBVersion:             "kb-v",
		LastEvaluated:         metav1.NewTime(time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)),
		Targets:               []TargetStatus{{Target: "1.36", Score: 88, Ready: true}},
		AgentVersion:          "v0.2.0",
	}
	if err := WriteStatus(ctx, dyn, DefaultName, st); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	obj, err := dyn.Resource(GVR()).Get(ctx, DefaultName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		t.Fatalf("status not written: found=%v err=%v", found, err)
	}
	var back Status
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &back); err != nil {
		t.Fatalf("decode written status: %v", err)
	}
	if back.Targets[0].Score != 88 || !back.Targets[0].Ready || back.KBVersion != "kb-v" {
		t.Errorf("written status = %+v, want score 88 ready kb-v", back)
	}
}

func TestWriteStatusRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	dyn := newDynFake(newCRObject(DefaultName))
	conflicts := 0
	dyn.PrependReactor("update", Plural, func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua := action.(k8stesting.UpdateAction)
		if ua.GetSubresource() != "status" {
			return false, nil, nil
		}
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: Group, Resource: Plural}, DefaultName, errors.New("rv mismatch"))
		}
		return false, nil, nil // fall through to the default reactor
	})
	if err := WriteStatus(ctx, dyn, DefaultName, Status{AgentVersion: "v0.2.0"}); err != nil {
		t.Fatalf("WriteStatus did not retry past one conflict: %v", err)
	}
	if conflicts != 1 {
		t.Errorf("conflict reactor fired %d times, want 1", conflicts)
	}
}

func TestWriteStatusMissingObject(t *testing.T) {
	dyn := newDynFake()
	if err := WriteStatus(context.Background(), dyn, DefaultName, Status{}); err == nil {
		t.Fatal("WriteStatus on absent object: want error, got nil")
	}
}
