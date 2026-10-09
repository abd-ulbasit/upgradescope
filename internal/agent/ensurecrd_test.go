package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

// #238: one failed CRD check at startup must not leave an older schema in
// place (pruning the status fields it lacks) until the pod restarts. Each
// tick tries again until it succeeds, and says so meanwhile, on the tick
// line and in status.notAssessed, without failing the tick.
func TestTickRetriesTheCRDUpgradeUntilItSucceeds(t *testing.T) {
	dyn := fakeDyn()
	r := testRunner(t, dyn, "")
	calls, failing := 0, true
	r.ensureCRD = func(context.Context) error {
		calls++
		if failing {
			return errors.New("get ClusterReadiness CRD: the server is currently unable to handle the request")
		}
		return nil
	}
	ctx := context.Background()
	if err := r.tick(ctx); err != nil {
		t.Fatalf("tick with the CRD check failing: %v, want the tick not failed", err)
	}
	if r.last.crdErr == nil {
		t.Error("tick report has no CRD error")
	}
	st := readCRStatus(t, dyn, crd.DefaultName)
	if len(st.NotAssessed) == 0 || !strings.Contains(st.NotAssessed[0], "ClusterReadiness CRD") {
		t.Errorf("notAssessed = %q, want the CRD note first", st.NotAssessed)
	}

	failing = false
	if err := r.tick(ctx); err != nil || r.last.crdErr != nil {
		t.Fatalf("tick after the CRD check succeeds: err %v, crdErr %v", err, r.last.crdErr)
	}
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("CRD checked %d times, want 2: until it succeeds, then no more", calls)
	}
	for _, n := range readCRStatus(t, dyn, crd.DefaultName).NotAssessed {
		if strings.Contains(n, "ClusterReadiness CRD") {
			t.Errorf("notAssessed still has %q once the CRD is up to date", n)
		}
	}
}

// End to end through Run: the startup check fails once, the first tick
// installs the CRD.
func TestRunRetriesEnsureCRDOnTheFirstTick(t *testing.T) {
	apiext := fakeAPIExt()
	gets := 0
	apiext.PrependReactor("get", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
		if gets++; gets == 1 {
			return true, nil, apierrors.NewServiceUnavailable("apiserver starting")
		}
		return false, nil, nil
	})
	runOneTick(t, apiext, Config{})
	if _, err := apiext.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.Background(), "clusterreadinesses.upgradescope.dev", metav1.GetOptions{}); err != nil {
		t.Errorf("the CRD was not installed by the tick after a failed startup check: %v", err)
	}
}

// An apiserver that hangs on the startup CRD check does not hold the
// first tick back: the check gets startupCRDTimeout, then falls back to
// the per-tick retry, and the tick writes the status meanwhile. A fake
// clientset ignores contexts, so the hang is a real HTTP server's.
func TestRunStartupCRDCheckIsBounded(t *testing.T) {
	old := startupCRDTimeout
	startupCRDTimeout = 100 * time.Millisecond
	defer func() { startupCRDTimeout = old }()
	var first atomic.Bool
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			<-r.Context().Done() // the startup GET: hang until the client gives up
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer hung.Close()
	apiext, err := apiextensionsclient.NewForConfig(&rest.Config{Host: hung.URL})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	dyn := fakeDyn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, fakeClients(t, "v1.35.2"), dyn, apiext, mustKB(t),
			Config{Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), crd.DefaultName, metav1.GetOptions{}); err == nil {
			if _, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the first tick never wrote the status behind a hung startup CRD check")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	var warned bool
	for _, l := range logs.lines(t) {
		if l["level"] == "WARN" && strings.Contains(l["msg"].(string), "retried every tick") &&
			strings.Contains(fmt.Sprint(l["err"]), "deadline") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no startup WARN naming the deadline: %v", logs.lines(t))
	}
}
