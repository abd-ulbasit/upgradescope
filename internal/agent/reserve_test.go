package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// ctxDyn is a dynamic client that honours its context the way a real one
// does (the fake ignores it): a call on a done context fails at once with
// the context's error, and a call whose verb is in stall blocks until its
// context is done, like a request to an apiserver that never answers.
// With served set, every call 404s while it returns false, as the
// apiserver answers for a CRD that is not Established.
type ctxDyn struct {
	dynamic.Interface
	stall  map[string]bool // "get", "create", "update", "update-status", "patch"
	served func() bool
}

func (d ctxDyn) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return ctxRes{d.Interface.Resource(gvr), gvr, d.stall, d.served}
}

type ctxRes struct {
	dynamic.NamespaceableResourceInterface
	gvr    schema.GroupVersionResource
	stall  map[string]bool
	served func() bool
}

func (r ctxRes) check(ctx context.Context, verb string) error {
	if r.stall[verb] {
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.served != nil && !r.served() {
		return apierrors.NewNotFound(r.gvr.GroupResource(), "")
	}
	return nil
}

func (r ctxRes) Get(ctx context.Context, name string, opts metav1.GetOptions, sub ...string) (*unstructured.Unstructured, error) {
	if err := r.check(ctx, "get"); err != nil {
		return nil, err
	}
	return r.NamespaceableResourceInterface.Get(ctx, name, opts, sub...)
}

func (r ctxRes) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, sub ...string) (*unstructured.Unstructured, error) {
	if err := r.check(ctx, "create"); err != nil {
		return nil, err
	}
	return r.NamespaceableResourceInterface.Create(ctx, obj, opts, sub...)
}

func (r ctxRes) Update(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions, sub ...string) (*unstructured.Unstructured, error) {
	if err := r.check(ctx, "update"); err != nil {
		return nil, err
	}
	return r.NamespaceableResourceInterface.Update(ctx, obj, opts, sub...)
}

func (r ctxRes) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	if err := r.check(ctx, "update-status"); err != nil {
		return nil, err
	}
	return r.NamespaceableResourceInterface.UpdateStatus(ctx, obj, opts)
}

func (r ctxRes) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, sub ...string) (*unstructured.Unstructured, error) {
	if err := r.check(ctx, "patch"); err != nil {
		return nil, err
	}
	return r.NamespaceableResourceInterface.Patch(ctx, name, pt, data, opts, sub...)
}

// slowCollect makes the runner's collection take all the time it is
// given, as a collector step does when /metrics never answers and
// --request-timeout does not end it first, and records the deadline it
// was given.
func slowCollect(r *runner) *time.Time {
	var mu sync.Mutex
	var deadline time.Time
	collect := r.collectFn
	r.collectFn = func(ctx context.Context) inventory.Inventory {
		inv := collect(ctx)
		mu.Lock()
		deadline, _ = ctx.Deadline()
		mu.Unlock()
		<-ctx.Done()
		return inv
	}
	return &deadline
}

func TestTickReserve(t *testing.T) {
	for _, tc := range []struct{ budget, want time.Duration }{
		{30 * time.Second, 15 * time.Second}, // --interval 1m: half
		{50 * time.Second, 25 * time.Second},
		{time.Minute, 30 * time.Second},
		{5 * time.Minute, 30 * time.Second}, // the default 10m interval
	} {
		if got := tickReserve(tc.budget); got != tc.want {
			t.Errorf("tickReserve(%v) = %v, want %v", tc.budget, got, tc.want)
		}
	}
}

// #238: a collection that uses all the time it is given must still leave
// the tick time to write the status and push. Collection gets the tick
// deadline minus the reserve; the status write and the push run after it,
// on time of their own.
func TestTickWithASlowCollectionStillWritesTheStatusAndPushes(t *testing.T) {
	fake := fakeDyn().(*dynamicfake.FakeDynamicClient)
	srv := newSnapServer(t)
	r := testRunner(t, ctxDyn{Interface: fake}, srv.srv.URL)
	const budget = 3 * time.Second // a 1.5s reserve: 750ms for the status write, room on a loaded runner
	r.tickBudget = budget
	collectDeadline := slowCollect(r)

	start := time.Now()
	rep := r.runTick(context.Background())
	if rep.err != nil || rep.pushErr != nil {
		t.Fatalf("tick err = %v, push err = %v; want both nil after a slow collection", rep.err, rep.pushErr)
	}
	if left := start.Add(budget).Sub(*collectDeadline); left < tickReserve(budget)-50*time.Millisecond {
		t.Errorf("collection ended %v before the tick deadline, want the %v reserve", left, tickReserve(budget))
	}
	if st := readCRStatus(t, fake, crd.DefaultName); len(st.Targets) != 1 {
		t.Errorf("status targets = %+v, want the evaluated target", st.Targets)
	}
	if srv.count() != 1 {
		t.Errorf("%d pushes, want 1", srv.count())
	}
}

// The status write has its own slice of the reserve, and the stale marker
// its own after it: a status write that runs out its time still leaves
// the marker, and the push, time to run.
func TestTickWithAStalledStatusWriteStillMarksTheCR(t *testing.T) {
	fake := fakeDyn().(*dynamicfake.FakeDynamicClient)
	srv := newSnapServer(t)
	r := testRunner(t, ctxDyn{Interface: fake, stall: map[string]bool{"update-status": true}}, srv.srv.URL)
	r.tickBudget = 3 * time.Second
	slowCollect(r)

	rep := r.runTick(context.Background())
	if rep.err == nil || !strings.Contains(rep.err.Error(), "update clusterreadiness") {
		t.Fatalf("tick err = %v, want the status write's", rep.err)
	}
	if strings.Contains(rep.err.Error(), crd.StatusErrorAnnotation) {
		t.Errorf("tick err = %v: the marker failed too, want it written on its own time", rep.err)
	}
	if v, ok := statusErrorAnnotation(t, fake); !ok || !strings.Contains(v, "deadline") {
		t.Errorf("status-error annotation = %q (%v), want the write's deadline as the reason", v, ok)
	}
	if rep.pushErr != nil || srv.count() != 1 {
		t.Errorf("push err = %v, %d pushes; want the push sent", rep.pushErr, srv.count())
	}
}

// #238 review: a CRD deleted while the agent runs (the CR goes with it) is
// created again by the tick's CRD check, the one at startup having failed,
// which then waits up to 10s for it to be Established: longer than the
// whole status slice at --interval 1m when collection runs out its time
// (7.5s), so the spec read used to fail and the CR was only marked. The
// check now has its own part of the slice and the calls after it keep a
// quarter of the reserve; a CRD Established within the check's part gets
// this tick's status written. Scaled down from the 30s tick of --interval
// 1m, as the other reserve tests are. The CR's endpoint 404s until the
// CRD is Established.
func TestTickRecreatingTheCRDStillWritesTheStatus(t *testing.T) {
	const establishedAfter = 150 * time.Millisecond
	var createdAt atomic.Int64
	established := func() bool {
		at := createdAt.Load()
		return at != 0 && time.Since(time.Unix(0, at)) >= establishedAfter
	}
	apiext := apiextfake.NewSimpleClientset() // the CRD was deleted
	apiext.PrependReactor("create", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
		createdAt.CompareAndSwap(0, time.Now().UnixNano())
		return false, nil, nil // stored as created: not Established
	})
	apiext.PrependReactor("get", "customresourcedefinitions", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := apiext.Tracker().Get(a.GetResource(), "", a.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		c := obj.(*apiextensionsv1.CustomResourceDefinition).DeepCopy()
		if established() {
			c.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
				{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
			}
		}
		return true, c, nil
	})

	fake := fakeDyn().(*dynamicfake.FakeDynamicClient)
	srv := newSnapServer(t)
	r := testRunner(t, ctxDyn{Interface: fake, served: established}, srv.srv.URL)
	const budget = 4 * time.Second // a 2s reserve: the CRD check ends 1.5s before the deadline
	r.tickBudget = budget
	slowCollect(r)
	var checkDeadline time.Time
	r.ensureCRD = func(ctx context.Context) error {
		checkDeadline, _ = ctx.Deadline()
		return crd.EnsureCRD(ctx, apiext)
	}

	start := time.Now()
	rep := r.runTick(context.Background())
	if rep.err != nil || rep.crdErr != nil || rep.pushErr != nil {
		t.Fatalf("tick err = %v, CRD err = %v, push err = %v; want none once the CRD is Established in time",
			rep.err, rep.crdErr, rep.pushErr)
	}
	if want := 3 * tickReserve(budget) / 4; start.Add(budget).Sub(checkDeadline) < want-50*time.Millisecond {
		t.Errorf("CRD check deadline %v before the tick deadline, want %v: the status calls keep a quarter of the reserve",
			start.Add(budget).Sub(checkDeadline), want)
	}
	if r.ensureCRD != nil {
		t.Error("the CRD check is still retried after it succeeded")
	}
	st := readCRStatus(t, fake, crd.DefaultName)
	if len(st.Targets) != 1 {
		t.Errorf("status targets = %+v, want the evaluated target", st.Targets)
	}
	for _, n := range st.NotAssessed {
		if strings.Contains(n, "ClusterReadiness CRD") {
			t.Errorf("notAssessed has %q, want no CRD note once it is re-created", n)
		}
	}
	if srv.count() != 1 {
		t.Errorf("%d pushes, want 1", srv.count())
	}
}

// A CRD check that hangs (its patch held by a slow admission webhook, say)
// runs out only its own part of the status slice: the spec read and the
// status write still run, on the installed schema, the status says why
// the CRD is not up to date, and the push is sent.
func TestTickWithAHungCRDCheckStillWritesTheStatus(t *testing.T) {
	fake := fakeDyn().(*dynamicfake.FakeDynamicClient)
	srv := newSnapServer(t)
	r := testRunner(t, ctxDyn{Interface: fake}, srv.srv.URL)
	r.tickBudget = 3 * time.Second
	slowCollect(r)
	r.ensureCRD = func(ctx context.Context) error {
		<-ctx.Done()
		return fmt.Errorf("apply ClusterReadiness CRD: %w", ctx.Err())
	}

	rep := r.runTick(context.Background())
	if rep.err != nil || rep.pushErr != nil {
		t.Fatalf("tick err = %v, push err = %v; want both nil behind a hung CRD check", rep.err, rep.pushErr)
	}
	if !errors.Is(rep.crdErr, context.DeadlineExceeded) {
		t.Errorf("CRD err = %v, want its own deadline", rep.crdErr)
	}
	st := readCRStatus(t, fake, crd.DefaultName)
	if len(st.Targets) != 1 || len(st.NotAssessed) == 0 || !strings.Contains(st.NotAssessed[0], "ClusterReadiness CRD") {
		t.Errorf("status targets %+v, notAssessed %q; want the evaluated target and the CRD note first", st.Targets, st.NotAssessed)
	}
	if v, ok := statusErrorAnnotation(t, fake); ok {
		t.Errorf("status-error annotation %q on a tick whose status was written", v)
	}
	if srv.count() != 1 {
		t.Errorf("%d pushes, want 1", srv.count())
	}
}

// A stop during collection still cancels everything after it: the reserve
// is time, not a way around a SIGTERM. Nothing is written or pushed.
func TestTickStoppedMidCollectionWritesNothing(t *testing.T) {
	fake := fakeDyn().(*dynamicfake.FakeDynamicClient)
	srv := newSnapServer(t)
	r := testRunner(t, ctxDyn{Interface: fake}, srv.srv.URL)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	collect := r.collectFn
	r.collectFn = func(cctx context.Context) inventory.Inventory {
		inv := collect(cctx)
		stop()
		return inv
	}
	rep := r.runTick(ctx)
	if rep.err == nil {
		t.Error("tick stopped mid-collection: want its cancelled calls reported")
	}
	if a := fake.Actions(); len(a) != 0 {
		t.Errorf("apiserver actions after a stop = %v, want none", a)
	}
	if srv.count() != 0 {
		t.Errorf("%d pushes after a stop, want none", srv.count())
	}
}
