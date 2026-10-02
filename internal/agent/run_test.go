package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

// fakeAPIExt returns an apiextensions fake whose created CRDs immediately
// report Established=True, like a real apiserver — otherwise crd.EnsureCRD's
// establishment poll would run out its full timeout inside tests.
func fakeAPIExt() *apiextfake.Clientset {
	fc := apiextfake.NewSimpleClientset()
	fc.PrependReactor("create", "customresourcedefinitions",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			obj := action.(k8stesting.CreateAction).GetObject().(*apiextensionsv1.CustomResourceDefinition)
			obj.Status.Conditions = append(obj.Status.Conditions, apiextensionsv1.CustomResourceDefinitionCondition{
				Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue,
			})
			return false, nil, nil // fall through to the default tracker reactor
		})
	return fc
}

func TestJitterBounds(t *testing.T) {
	d := 10 * time.Minute
	lo, hi := 9*time.Minute, 11*time.Minute
	for i := 0; i < 200; i++ {
		got := jitter(d)
		if got < lo || got > hi {
			t.Fatalf("jitter(%v) = %v, outside [%v, %v]", d, got, lo, hi)
		}
	}
}

func TestRunInvalidConfig(t *testing.T) {
	err := Run(context.Background(), fakeClients(t, "v1.35.2"), fakeDyn(),
		fakeAPIExt(), mustKB(t), Config{Interval: time.Second})
	if err == nil {
		t.Fatal("Run with sub-minimum interval: want error")
	}
}

// runOneTick starts Run, waits for the first status write, cancels, and
// requires a clean (nil) return.
func runOneTick(t *testing.T, apiext *apiextfake.Clientset, cfg Config) {
	t.Helper()
	dyn := fakeDyn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, fakeClients(t, "v1.35.2"), dyn, apiext, mustKB(t), cfg) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), crd.DefaultName, metav1.GetOptions{})
		if err == nil {
			if _, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("first tick never wrote CRD status")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// --manage-crd=false (chart agent.manageCRD=false): the agent has no write
// RBAC on its CRD and must not touch the CRD at all, not even a read.
func TestRunSkipsCRDWhenNotManaged(t *testing.T) {
	apiext := fakeAPIExt()
	runOneTick(t, apiext, Config{SkipCRDManagement: true})
	if a := apiext.Actions(); len(a) != 0 {
		t.Errorf("apiextensions actions = %v, want none", a)
	}
}

// A missing CRD the agent may not create is a clear startup error, not a
// warning followed by a 404 on every tick.
func TestRunFailsWhenCRDMissingAndNotCreatable(t *testing.T) {
	apiext := apiextfake.NewClientset()
	apiext.PrependReactor("create", "customresourcedefinitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, "x", errors.New("RBAC"))
		})
	err := Run(context.Background(), fakeClients(t, "v1.35.2"), fakeDyn(), apiext, mustKB(t), Config{})
	if !errors.Is(err, crd.ErrCRDNotInstalled) {
		t.Fatalf("Run err = %v, want crd.ErrCRDNotInstalled", err)
	}
}

// TestRunFirstTickThenGracefulStop: Run ensures the CRD, ticks once
// synchronously before the first wait, then blocks on the (≥1m, jittered)
// timer. We poll the fake for the first tick's status write, cancel, and
// require a nil return. No timing dependence: the first tick happens before
// any timer, and 1m never elapses inside the test.
func TestRunFirstTickThenGracefulStop(t *testing.T) {
	dyn := fakeDyn()
	apiext := fakeAPIExt()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, fakeClients(t, "v1.35.2"), dyn, apiext, mustKB(t), Config{})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), crd.DefaultName, metav1.GetOptions{})
		if err == nil {
			if _, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("first tick never wrote CRD status")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// EnsureCRD ran at startup against the apiextensions fake.
	if _, err := apiext.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.Background(), "clusterreadinesses.upgradescope.dev", metav1.GetOptions{}); err != nil {
		t.Errorf("EnsureCRD did not install the CRD: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil (graceful stop)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// A stop that lands mid-tick cancels the tick's calls. That is a graceful
// stop, not a tick failure: no ERROR line, and one INFO line saying the
// agent is stopping.
func TestRunStopMidTickIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dyn := fakeDyn().(*dynamicfake.FakeDynamicClient)
	dyn.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel() // the stop arrives while the tick talks to the apiserver
		return true, nil, context.Canceled
	})
	logs := &syncBuffer{}
	cfg := Config{Logger: slog.New(slog.NewJSONHandler(logs, nil))}
	if err := Run(ctx, fakeClients(t, "v1.35.2"), dyn, fakeAPIExt(), mustKB(t), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := logs.lines(t)
	if failed := linesWithMsg(lines, msgTickFailed); len(failed) != 0 {
		t.Errorf("tick failed lines = %v, want none on a graceful stop", failed)
	}
	if stopping := linesWithMsg(lines, msgStopping); len(stopping) != 1 || stopping[0]["level"] != "INFO" {
		t.Errorf("stopping lines = %v, want one INFO line", stopping)
	}
}
