package crd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// Issue #199 (RB-07): an agent that cannot write the status marks the CR
// with an annotation naming when and why, in one short line, so the stale
// verdict is not shown unmarked; the next successful status write clears
// it. The chart grants patch on the CR itself, which status does not need.
func TestMarkStatusErrorAndWriteStatusClearsIt(t *testing.T) {
	ctx := context.Background()
	dyn := newDynFake(newCRObject(DefaultName))
	annotation := func() (string, bool) {
		obj, err := dyn.Resource(GVR()).Get(ctx, DefaultName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		v, ok := obj.GetAnnotations()[StatusErrorAnnotation]
		return v, ok
	}
	at := time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC)
	cause := errors.New("update clusterreadiness \"cluster\" status: clusterreadinesses.upgradescope.dev \"cluster\" is forbidden:\n\tUser cannot update resource \"clusterreadinesses/status\" " + strings.Repeat("x", 600))

	if err := MarkStatusError(ctx, dyn, DefaultName, cause, at); err != nil {
		t.Fatalf("MarkStatusError: %v", err)
	}
	got, ok := annotation()
	if !ok || !strings.HasPrefix(got, "2026-10-03T09:30:00Z ") || !strings.Contains(got, "is forbidden: User cannot update") {
		t.Fatalf("annotation = %q (%v), want the time then the reason", got, ok)
	}
	if strings.ContainsAny(got, "\n\t") || len(got) > 320 {
		t.Errorf("annotation is %d bytes with control characters %v, want one short line", len(got), strings.ContainsAny(got, "\n\t"))
	}

	// A second failure replaces it rather than stacking.
	if err := MarkStatusError(ctx, dyn, DefaultName, errors.New("second"), at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := annotation(); got != "2026-10-03T09:31:00Z second" {
		t.Errorf("annotation = %q, want the latest failure only", got)
	}

	if err := WriteStatus(ctx, dyn, DefaultName, Status{AgentVersion: "v0.2.0"}); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	if got, ok := annotation(); ok {
		t.Errorf("annotation %q survived a successful status write", got)
	}
}

// A write that succeeds on a CR that was never marked patches nothing: the
// agent writes status every tick.
func TestWriteStatusPatchesNothingWhenNotMarked(t *testing.T) {
	dyn := newDynFake(newCRObject(DefaultName))
	if err := WriteStatus(context.Background(), dyn, DefaultName, Status{}); err != nil {
		t.Fatal(err)
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "patch" {
			t.Errorf("unexpected %s of %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// With patch on the CR revoked too, no marker can be written: the error
// says so, and /readyz and the alert stay the only signals.
func TestMarkStatusErrorForbidden(t *testing.T) {
	dyn := newDynFake(newCRObject(DefaultName))
	dyn.PrependReactor("patch", Plural, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: Group, Resource: Plural}, DefaultName, errors.New("RBAC"))
	})
	if err := MarkStatusError(context.Background(), dyn, DefaultName, errors.New("x"), time.Now()); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v, want the forbidden patch", err)
	}
}

// A status write that succeeded but could not clear the marker of an
// earlier failure has still written the status: the error says which of
// the two failed, so the caller does not treat a current status as a
// failed write.
func TestWriteStatusTellsAFailedClearFromAFailedWrite(t *testing.T) {
	ctx := context.Background()
	dyn := newDynFake(newCRObject(DefaultName))
	if err := MarkStatusError(ctx, dyn, DefaultName, errors.New("earlier"), time.Now()); err != nil {
		t.Fatal(err)
	}
	dyn.PrependReactor("patch", Plural, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: Group, Resource: Plural}, DefaultName, errors.New("RBAC"))
	})

	err := WriteStatus(ctx, dyn, DefaultName, Status{AgentVersion: "v0.2.0"})
	if !errors.Is(err, ErrStatusErrorNotCleared) || !strings.Contains(err.Error(), StatusErrorAnnotation) {
		t.Fatalf("err = %v, want ErrStatusErrorNotCleared naming the annotation", err)
	}
	obj, gerr := dyn.Resource(GVR()).Get(ctx, DefaultName, metav1.GetOptions{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if v, _, _ := unstructured.NestedString(obj.Object, "status", "agentVersion"); v != "v0.2.0" {
		t.Errorf("status.agentVersion = %q, want the status written despite the failed clear", v)
	}

	// A failed status write is not that error.
	dyn.PrependReactor("update", Plural, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: Group, Resource: Plural + "/status"}, DefaultName, errors.New("RBAC"))
	})
	if err := WriteStatus(ctx, dyn, DefaultName, Status{}); err == nil || errors.Is(err, ErrStatusErrorNotCleared) {
		t.Errorf("err = %v, want the write error, not ErrStatusErrorNotCleared", err)
	}
}
