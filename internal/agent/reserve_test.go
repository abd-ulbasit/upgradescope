package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// ctxDyn is a dynamic client that honours its context the way a real one
// does (the fake ignores it): a call on a done context fails at once with
// the context's error, and a call whose verb is in stall blocks until its
// context is done, like a request to an apiserver that never answers.
type ctxDyn struct {
	dynamic.Interface
	stall map[string]bool // "get", "create", "update", "update-status", "patch"
}

func (d ctxDyn) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return ctxRes{d.Interface.Resource(gvr), d.stall}
}

type ctxRes struct {
	dynamic.NamespaceableResourceInterface
	stall map[string]bool
}

func (r ctxRes) check(ctx context.Context, verb string) error {
	if r.stall[verb] {
		<-ctx.Done()
	}
	return ctx.Err()
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
	const budget = time.Second
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
	r.tickBudget = time.Second
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
