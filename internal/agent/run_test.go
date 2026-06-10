package agent

import (
	"context"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
