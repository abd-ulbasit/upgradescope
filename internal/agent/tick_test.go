package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// fakeClients builds collect.Clients over a fake clientset reporting the given
// server version. Metadata/RESTClient stay nil — those capabilities degrade,
// which is exactly the "not assessed" path we want exercised.
func fakeClients(t *testing.T, serverVersion string) collect.Clients {
	t.Helper()
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: serverVersion}
	return collect.Clients{Kube: cs, Discovery: disc}
}

func fakeDyn(objects ...runtime.Object) dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crd.GVR(): crd.Kind + "List"},
		objects...,
	)
}

func mustKB(t *testing.T) kb.KB {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatalf("kb.Load: %v", err)
	}
	return k
}

func readCRStatus(t *testing.T, dyn dynamic.Interface, name string) crd.Status {
	t.Helper()
	obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR %q: %v", name, err)
	}
	raw, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		t.Fatalf("status not written: found=%v err=%v", found, err)
	}
	var st crd.Status
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

// snapServer records decoded push payloads and always answers 202.
type snapServer struct {
	mu       sync.Mutex
	payloads []pushPayload
	srv      *httptest.Server
}

func newSnapServer(t *testing.T) *snapServer {
	s := &snapServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("push body not gzipped: %v", err)
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		var pl pushPayload
		if err := json.NewDecoder(zr).Decode(&pl); err != nil {
			t.Errorf("push body not JSON: %v", err)
		}
		s.mu.Lock()
		s.payloads = append(s.payloads, pl)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"snapshotId": 1}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *snapServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.payloads)
}

func testRunner(t *testing.T, dyn dynamic.Interface, serverURL string) *runner {
	t.Helper()
	cfg := Config{ServerURL: serverURL, ServerToken: "tok"}
	if serverURL == "" {
		cfg.ServerToken = ""
	}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(fakeClients(t, "v1.35.2"), dyn, mustKB(t), cfg)
	if r.pusher != nil {
		r.pusher.sleep = func(time.Duration) {} // never really sleep in tests
	}
	return r
}

func TestTickWritesStatusWithDefaultTarget(t *testing.T) {
	ctx := context.Background()
	dyn := fakeDyn()
	srv := newSnapServer(t)
	r := testRunner(t, dyn, srv.srv.URL)

	if err := r.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	st := readCRStatus(t, dyn, crd.DefaultName)
	if st.ObservedServerVersion != "v1.35.2" {
		t.Errorf("ObservedServerVersion = %q", st.ObservedServerVersion)
	}
	if len(st.Targets) != 1 || st.Targets[0].Target != "1.36" {
		t.Fatalf("Targets = %+v, want default next minor 1.36", st.Targets)
	}
	if st.Targets[0].Score < 0 || st.Targets[0].Score > 100 {
		t.Errorf("Score = %d, want 0..100", st.Targets[0].Score)
	}
	if st.KBVersion == "" || st.AgentVersion == "" {
		t.Errorf("KBVersion/AgentVersion empty: %+v", st)
	}
	if len(st.NotAssessed) == 0 {
		t.Error("nil Metadata client should surface notAssessed entries")
	}
}

func TestTickPushesWithClusterIDWhenNameUnset(t *testing.T) {
	ctx := context.Background()
	srv := newSnapServer(t)
	r := testRunner(t, fakeDyn(), srv.srv.URL)
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 1 {
		t.Fatalf("pushes = %d, want 1", srv.count())
	}
	pl := srv.payloads[0]
	if pl.ClusterName != "uid-123" {
		t.Errorf("ClusterName = %q, want cluster UID fallback uid-123", pl.ClusterName)
	}
	if pl.SchemaVersion != 1 || pl.KBVersion == "" || len(pl.Inventory) == 0 {
		t.Errorf("payload = %+v", pl)
	}
}

func TestTickDedupsUnchangedInventory(t *testing.T) {
	ctx := context.Background()
	srv := newSnapServer(t)
	r := testRunner(t, fakeDyn(), srv.srv.URL)
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cur := base
	r.now = func() time.Time { return cur }

	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	cur = cur.Add(10 * time.Minute)
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 1 {
		t.Fatalf("pushes = %d, want 1 (unchanged inventory deduped)", srv.count())
	}

	// ForceSyncEvery (1h default) elapsed → push despite unchanged hash.
	cur = cur.Add(2 * time.Hour)
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 2 {
		t.Fatalf("pushes = %d, want 2 (force sync elapsed)", srv.count())
	}
}

func TestTickHonorsSpecTargets(t *testing.T) {
	ctx := context.Background()
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.Group + "/" + crd.Version,
		"kind":       crd.Kind,
		"metadata":   map[string]interface{}{"name": crd.DefaultName},
		"spec":       map[string]interface{}{"targets": []interface{}{"1.36", "1.37"}},
	}}
	dyn := fakeDyn(cr)
	r := testRunner(t, dyn, "") // CRD-only mode
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	st := readCRStatus(t, dyn, crd.DefaultName)
	if len(st.Targets) != 2 || st.Targets[0].Target != "1.36" || st.Targets[1].Target != "1.37" {
		t.Fatalf("Targets = %+v, want spec targets [1.36 1.37]", st.Targets)
	}
}

func TestTickCRDOnlyModeNoPusher(t *testing.T) {
	r := testRunner(t, fakeDyn(), "")
	if r.pusher != nil {
		t.Fatal("pusher built without ServerURL")
	}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("CRD-only tick: %v", err)
	}
}

// fakeClientsInventory collects a realistic inventory from the fakes — the
// hash test must exercise the real wire shape, not a hand-built struct.
func fakeClientsInventory(t *testing.T) inventory.Inventory {
	t.Helper()
	return collect.Collect(context.Background(), fakeClients(t, "v1.35.2"), mustKB(t), collect.Options{TeamLabel: "team"})
}

func TestSnapshotHashIgnoresCollectedAt(t *testing.T) {
	inv := fakeClientsInventory(t)
	h1, _, err := snapshotHash(inv)
	if err != nil {
		t.Fatal(err)
	}
	inv.CollectedAt = inv.CollectedAt.Add(time.Hour)
	h2, _, err := snapshotHash(inv)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("hash changed when only CollectedAt changed — dedup would never fire")
	}
	inv.ServerVersion = "v1.99.0"
	h3, _, _ := snapshotHash(inv)
	if h3 == h1 {
		t.Error("hash did not change when content changed")
	}
}
