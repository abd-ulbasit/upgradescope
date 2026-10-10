package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/yaml"

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
		// One node: with none the versions capability is partial and required (#174).
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: serverVersion}}},
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
		r.pusher.wait = func(context.Context, time.Duration) error { return nil } // never really wait in tests
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

// gkeStandardEnd is the day GKE standard support ends for minor, from the
// embedded knowledge base: the test's clock is derived from the data, never
// the wall clock, so it does not break on the day a real calendar says a
// minor leaves support (#269: it did on 2027-04-11).
func gkeStandardEnd(t *testing.T, k kb.KB, minor string) time.Time {
	t.Helper()
	p, ok := k.Provider("gke")
	if !ok {
		t.Fatal("the knowledge base has no GKE calendar")
	}
	for _, w := range p.Versions {
		if w.Minor == minor {
			d, err := time.Parse("2006-01-02", w.StandardEnd)
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("the GKE calendar has no minor %s", minor)
	return time.Time{}
}

// Chart defaults (targets: []) on a GKE cluster: the default target derives
// from the vendor-suffixed server version and a score and verdict are
// written. The fakes leave api-usage unassessed (nil metadata client), so
// the verdict is unknown, never ready. The clock is injected, 120 days
// before GKE 1.35 leaves standard support (outside the 90-day warning
// window), and again the day after, when the support calendar's blocker
// makes the verdict blocked: the test no longer depends on the date it
// runs on.
func TestTickVendorServerVersionWritesScoreAndVerdict(t *testing.T) {
	k := mustKB(t)
	end := gkeStandardEnd(t, k, "1.35")
	for _, c := range []struct {
		name        string
		now         time.Time
		wantVerdict string
	}{
		{"before the support calendar's window", end.AddDate(0, 0, -120), "unknown"},
		{"after standard support ended", end.AddDate(0, 0, 1), "blocked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dyn := fakeDyn()
			cfg := Config{}
			if err := cfg.applyDefaults(); err != nil {
				t.Fatal(err)
			}
			r := newRunner(fakeClients(t, "v1.35.2-gke.1080000"), dyn, k, cfg)
			r.now = func() time.Time { return c.now }
			if err := r.tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			st := readCRStatus(t, dyn, crd.DefaultName)
			if len(st.Targets) != 1 || st.Targets[0].Target != "1.36" {
				t.Fatalf("Targets = %+v, want default next minor 1.36", st.Targets)
			}
			if got := st.Targets[0]; got.Verdict != c.wantVerdict || got.Ready {
				t.Errorf("target status verdict = %q ready = %v, want %s/false", got.Verdict, got.Ready, c.wantVerdict)
			}
		})
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

// A CR listing the same target twice gets one evaluation, one status row,
// and a /metrics endpoint that still serves.
func TestTickDuplicateSpecTargetsEvaluatedOnce(t *testing.T) {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.Group + "/" + crd.Version,
		"kind":       crd.Kind,
		"metadata":   map[string]interface{}{"name": crd.DefaultName},
		"spec":       map[string]interface{}{"targets": []interface{}{"1.36", "1.36"}},
	}}
	dyn := fakeDyn(cr)
	r := testRunner(t, dyn, "") // CRD-only mode
	if err := r.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.last.reports) != 1 {
		t.Errorf("reports = %d, want 1", len(r.last.reports))
	}
	if st := readCRStatus(t, dyn, crd.DefaultName); len(st.Targets) != 1 || st.Targets[0].Target != "1.36" {
		t.Errorf("status targets = %+v, want one row for 1.36", st.Targets)
	}

	o := newObserver(slog.New(slog.NewTextHandler(io.Discard, nil)), mustKB(t), 10*time.Minute)
	o.record(r.last)
	if code, body := serve(t, o.handler(), "/metrics"); code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200\n%s", code, body)
	}
}

// With no spec targets and no usable server version nothing is evaluated.
// The CR still says why (Ready=Unknown, notAssessed), but the tick fails:
// with no report there is no verdict series for the unknown-verdict alert,
// so the failed-tick metrics and /readyz are what surface it.
func TestTickWithoutTargetsFails(t *testing.T) {
	dyn := fakeDyn()
	cfg := Config{}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(fakeClients(t, "garbage"), dyn, mustKB(t), cfg)
	if err := r.tick(context.Background()); err == nil {
		t.Fatal("tick with no resolvable target: want an error")
	}
	if r.last.err == nil || len(r.last.reports) != 0 {
		t.Errorf("tick report = %+v, want a tick error and no reports", r.last)
	}
	st := readCRStatus(t, dyn, crd.DefaultName)
	if len(st.Targets) != 0 || !slices.ContainsFunc(st.NotAssessed, func(s string) bool { return strings.Contains(s, "garbage") }) {
		t.Errorf("status = %+v, want no targets and a notAssessed entry naming the server version", st)
	}
}

// A spec edit that lands after the tick read the spec must not be claimed
// as observed: the status says the generation that was evaluated, so Argo
// CD keeps waiting for the next tick.
func TestTickStampsTheGenerationItEvaluated(t *testing.T) {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.Group + "/" + crd.Version,
		"kind":       crd.Kind,
		"metadata":   map[string]interface{}{"name": crd.DefaultName, "generation": int64(4)},
		"spec":       map[string]interface{}{"targets": []interface{}{"1.36"}},
	}}
	dyn := fakeDyn(cr).(*dynamicfake.FakeDynamicClient)
	updates := 0
	dyn.PrependReactor("update", crd.Plural, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "status" {
			return false, nil, nil
		}
		if updates++; updates == 1 { // someone edited the spec since the tick read it: the stale write is refused
			edited := cr.DeepCopy()
			edited.SetGeneration(5)
			_ = unstructured.SetNestedStringSlice(edited.Object, []string{"1.37"}, "spec", "targets")
			if err := dyn.Tracker().Update(crd.GVR(), edited, ""); err != nil {
				t.Errorf("simulate the edit: %v", err)
			}
			return true, nil, apierrors.NewConflict(crd.GVR().GroupResource(), crd.DefaultName, errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	r := testRunner(t, dyn, "")
	if err := r.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if updates != 2 {
		t.Errorf("%d status writes, want 2: the refused one and its retry over a fresh read", updates)
	}
	st := readCRStatus(t, dyn, crd.DefaultName)
	if st.ObservedGeneration != 4 || len(st.Targets) != 1 || st.Targets[0].Target != "1.36" {
		t.Errorf("status observedGeneration=%d targets=%+v, want 4 and the evaluated 1.36", st.ObservedGeneration, st.Targets)
	}
}

// readCRSpecTargets returns spec.targets of the named CR.
func readCRSpecTargets(t *testing.T, dyn dynamic.Interface, name string) []string {
	t.Helper()
	spec, _, found, err := crd.ReadSpec(context.Background(), dyn, name)
	if err != nil || !found {
		t.Fatalf("read spec of %q: found=%v err=%v", name, found, err)
	}
	return spec.Targets
}

func targetsRunner(t *testing.T, dyn dynamic.Interface, targets ...string) *runner {
	t.Helper()
	cfg := Config{Targets: targets}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	return newRunner(fakeClients(t, "v1.35.2"), dyn, mustKB(t), cfg)
}

// The chart passes agent.targets as --targets instead of rendering the CR,
// so Helm never owns an object the agent also creates. A missing CR is
// created with the flag's targets.
func TestTickCreatesCRWithFlagTargets(t *testing.T) {
	dyn := fakeDyn()
	r := targetsRunner(t, dyn, "1.37", "1.38")
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := readCRSpecTargets(t, dyn, crd.DefaultName); !slices.Equal(got, []string{"1.37", "1.38"}) {
		t.Errorf("spec.targets = %v, want [1.37 1.38]", got)
	}
	requireCRValidAgainstCRD(t, dyn, crd.DefaultName)
	st := readCRStatus(t, dyn, crd.DefaultName)
	if len(st.Targets) != 2 || st.Targets[0].Target != "1.37" {
		t.Errorf("status targets = %+v, want 1.37 and 1.38", st.Targets)
	}
}

// helm upgrade --set agent.targets={1.37} after a default install: the CR
// already exists (agent-created, empty spec or kubectl-edited targets) and
// the flag value wins.
func TestTickReconcilesExistingSpecToFlagTargets(t *testing.T) {
	dyn := fakeDyn(&unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.Group + "/" + crd.Version,
		"kind":       crd.Kind,
		"metadata":   map[string]interface{}{"name": crd.DefaultName},
		"spec":       map[string]interface{}{"targets": []interface{}{"1.36"}},
	}})
	r := targetsRunner(t, dyn, "1.37")
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := readCRSpecTargets(t, dyn, crd.DefaultName); !slices.Equal(got, []string{"1.37"}) {
		t.Errorf("spec.targets = %v, want flag value [1.37]", got)
	}
	requireCRValidAgainstCRD(t, dyn, crd.DefaultName)
	st := readCRStatus(t, dyn, crd.DefaultName)
	if len(st.Targets) != 1 || st.Targets[0].Target != "1.37" {
		t.Errorf("status targets = %+v, want only 1.37", st.Targets)
	}
}

// Once spec.targets matches the flag, ticks do not rewrite the spec.
func TestTickNoSpecWriteWhenFlagTargetsMatch(t *testing.T) {
	dyn := fakeDyn()
	r := targetsRunner(t, dyn, "1.37")
	ctx := context.Background()
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	fake := dyn.(*dynamicfake.FakeDynamicClient)
	fake.ClearActions()
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	for _, a := range fake.Actions() {
		if a.GetVerb() == "patch" || (a.GetVerb() == "update" && a.GetSubresource() == "") {
			t.Errorf("unexpected spec write %s on an in-sync CR", a.GetVerb())
		}
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

// The apiserver that answered the /metrics scrape is not the cluster: an
// agent whose connection moves between HA apiservers, started at
// different times, sends no new snapshot for it (#204).
func TestSnapshotHashIgnoresAPIServerStart(t *testing.T) {
	inv := fakeClientsInventory(t)
	h1, _, err := snapshotHash(inv)
	if err != nil {
		t.Fatal(err)
	}
	inv.APIServerStartTime = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	h2, raw, err := snapshotHash(inv)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("hash changed when only APIServerStartTime changed — an HA move would push every tick")
	}
	if !strings.Contains(string(raw), `"apiServerStartTime":"2026-08-01T09:00:00Z"`) {
		t.Errorf("wire JSON lacks the start time: %s", raw)
	}
}

// requireCRValidAgainstCRD validates the stored CR against the embedded
// CRD's openAPIV3Schema, as the apiserver would on create/patch. The dynamic
// fake stores anything, so without this a value the real apiserver rejects
// with 422 Invalid (e.g. spec.targets "v1.38") passes every other test.
func requireCRValidAgainstCRD(t *testing.T, dyn dynamic.Interface, name string) {
	t.Helper()
	var def apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(crd.Manifest, &def); err != nil {
		t.Fatalf("parse embedded CRD: %v", err)
	}
	// The schema is plain OpenAPI v3 (no CEL, no int-or-string), so a JSON
	// round-trip gives the spec.Schema kube-openapi validates with.
	raw, err := json.Marshal(def.Spec.Versions[0].Schema.OpenAPIV3Schema)
	if err != nil {
		t.Fatalf("encode CRD schema: %v", err)
	}
	var crSchema spec.Schema
	if err := json.Unmarshal(raw, &crSchema); err != nil {
		t.Fatalf("decode CRD schema: %v", err)
	}
	obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR %q: %v", name, err)
	}
	if err := validate.AgainstSchema(&crSchema, obj.Object, strfmt.Default); err != nil {
		t.Errorf("CR %q would be rejected by the apiserver: %v", name, err)
	}
}

// --targets accepts anything inventory.ParseTarget does ("v1.38", "1.37.2"),
// but the CRD pins spec.targets items to MAJOR.MINOR. The agent writes the
// normalized minor, on both the create and the patch path, so the apiserver
// accepts the CR.
func TestTickWritesFlagTargetsTheCRDAccepts(t *testing.T) {
	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.Group + "/" + crd.Version,
		"kind":       crd.Kind,
		"metadata":   map[string]interface{}{"name": crd.DefaultName},
		"spec":       map[string]interface{}{"targets": []interface{}{"1.36"}},
	}}
	for name, dyn := range map[string]dynamic.Interface{
		"create": fakeDyn(),
		"patch":  fakeDyn(existing),
	} {
		t.Run(name, func(t *testing.T) {
			r := targetsRunner(t, dyn, "v1.38", "1.37.2")
			if err := r.tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if got := readCRSpecTargets(t, dyn, crd.DefaultName); !slices.Equal(got, []string{"1.38", "1.37"}) {
				t.Errorf("spec.targets = %v, want [1.38 1.37]", got)
			}
			requireCRValidAgainstCRD(t, dyn, crd.DefaultName)
		})
	}
}

// A tick reads its ClusterReadiness once and writes the status over what it
// read (#228): a steady tick asks for one GET and one status UPDATE, and
// creates the object only on the tick that finds it missing.
func TestTickReadsTheClusterReadinessOnce(t *testing.T) {
	dyn := fakeDyn().(*dynamicfake.FakeDynamicClient)
	r := testRunner(t, dyn, "")
	verbs := func() string {
		var out []string
		for _, a := range dyn.Actions() {
			v := a.GetVerb()
			if a.GetSubresource() != "" {
				v += "/" + a.GetSubresource()
			}
			out = append(out, v)
		}
		dyn.ClearActions()
		return strings.Join(out, " ")
	}
	for tick, want := range []string{"get create get update/status", "get update/status", "get update/status"} {
		if err := r.tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick+1, err)
		}
		if got := verbs(); got != want {
			t.Errorf("tick %d: ClusterReadiness requests %q, want %q", tick+1, got, want)
		}
	}
	if err := dyn.Resource(crd.GVR()).Delete(context.Background(), crd.DefaultName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	dyn.ClearActions()
	if err := r.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := verbs(), "get create get update/status"; got != want {
		t.Errorf("after the object was deleted: %q, want %q (recreated)", got, want)
	}
}
