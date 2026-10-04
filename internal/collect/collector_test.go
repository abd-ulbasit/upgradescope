package collect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func TestRunStepsDegradesFailedCapabilityAndKeepsOthers(t *testing.T) {
	inv := inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
	ss := []step{
		{cap: inventory.CapVersions, run: func(_ context.Context, inv *inventory.Inventory) error {
			inv.ServerVersion = "v1.34.2"
			return nil
		}},
		{cap: inventory.CapHelm, run: func(_ context.Context, _ *inventory.Inventory) error {
			return errors.New(`secrets is forbidden: User "scanner" cannot list resource "secrets"`)
		}},
		{cap: inventory.CapAddOns, run: func(_ context.Context, inv *inventory.Inventory) error {
			inv.AddOns = []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"ingress-nginx"}, Source: "image"}}
			return nil
		}},
	}
	runSteps(context.Background(), &inv, ss)

	if got := inv.Capabilities[inventory.CapVersions]; !got.Available || got.Reason != "" {
		t.Errorf("versions capability = %+v, want available with no reason", got)
	}
	helm := inv.Capabilities[inventory.CapHelm]
	if helm.Available {
		t.Error("helm capability should be degraded after a forbidden error")
	}
	if helm.Reason == "" {
		t.Error("degraded capability must carry the error as Reason")
	}
	if inv.ServerVersion != "v1.34.2" {
		t.Errorf("ServerVersion = %q; data from the successful step before the failure must persist", inv.ServerVersion)
	}
	if len(inv.AddOns) != 1 {
		t.Errorf("AddOns = %+v; steps after a failed step must still run", inv.AddOns)
	}
}

func TestRunStepsPartialErrorKeepsCapabilityAvailable(t *testing.T) {
	inv := inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
	runSteps(context.Background(), &inv, []step{
		{cap: inventory.CapAPIUsage, run: func(context.Context, *inventory.Inventory) error {
			return partialError{msg: "list policy/v1beta1 podsecuritypolicies: forbidden", incomplete: true,
				skipped: []string{"policy/v1beta1 PodSecurityPolicy"}}
		}},
		{cap: inventory.CapHelm, run: func(context.Context, *inventory.Inventory) error {
			return partialError{msg: "helm releases: 2 via secrets, 0 via configmaps"}
		}},
	})
	want := map[inventory.Capability]inventory.CapabilityStatus{
		// Some data was not read: available, but partial, naming what.
		inventory.CapAPIUsage: {Available: true, Partial: true, Reason: "list policy/v1beta1 podsecuritypolicies: forbidden",
			Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}},
		// An informational reason: everything was read.
		inventory.CapHelm: {Available: true, Reason: "helm releases: 2 via secrets, 0 via configmaps"},
	}
	if !reflect.DeepEqual(inv.Capabilities, want) {
		t.Errorf("capabilities = %+v\nwant           %+v", inv.Capabilities, want)
	}
}

// A step's reason joins one failure per resource it could not read, and
// objects carry their ignore annotations whole: the default chart grants
// no custom resources, so each CRD at a deprecated version adds a ~200-byte
// forbidden list. runSteps cuts both to the inventory limits, or 400 such
// CRDs or one long annotation would get every push of the agent refused.
func TestRunStepsCutsFreeTextToTheLimits(t *testing.T) {
	inv := inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
	var failures []string
	for i := range 400 {
		failures = append(failures, fmt.Sprintf(`list example%03d.io/v1beta1 widgets: widgets.example%03d.io is forbidden: User "system:serviceaccount:upgradescope:upgradescope-agent" cannot list resource "widgets" in API group "example%03d.io" at the cluster scope`, i, i, i))
	}
	runSteps(context.Background(), &inv, []step{
		{cap: inventory.CapAPIUsage, run: func(_ context.Context, inv *inventory.Inventory) error {
			inv.APIUsage = []inventory.APIUsage{{Version: "v1", Kind: "ConfigMap", Count: 1, Objects: []inventory.ObjectRef{{
				Name: "cm", Ignore: "deprecated-api", IgnoreReason: strings.Repeat("why ", 64<<10)}}}}
			return nil
		}},
		{cap: inventory.CapCRDs, run: func(context.Context, *inventory.Inventory) error {
			return partialError{msg: strings.Join(failures, "; "), incomplete: true}
		}},
	})
	if err := inv.ValidateLimits(); err != nil {
		t.Fatalf("ValidateLimits() = %v, want the collected inventory within the limits", err)
	}
	if r := inv.Capabilities[inventory.CapCRDs].Reason; !strings.HasPrefix(r, failures[0]) {
		t.Errorf("reason starts %.80q, want the first failure", r)
	}
}

func TestCollectDefaults(t *testing.T) {
	inv := Collect(context.Background(), Clients{}, kb.KB{}, Options{})
	if inv.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", inv.SchemaVersion)
	}
	if inv.Capabilities == nil {
		t.Error("Capabilities map must be initialized")
	}
	if inv.CollectedAt.IsZero() {
		t.Error("CollectedAt must be set")
	}
	if inv.Source != inventory.SourceCluster {
		t.Errorf("Source = %q, want %q", inv.Source, inventory.SourceCluster)
	}
	if inv.CollectorSchema != inventory.CurrentCollectorSchema {
		t.Errorf("CollectorSchema = %d, want %d", inv.CollectorSchema, inventory.CurrentCollectorSchema)
	}
}

// Offline inventories must say so: the engine does not require the versions
// capability for them.
func TestManifestInventoriesAreFilesSource(t *testing.T) {
	inv, err := CollectManifests(strings.NewReader("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Source != inventory.SourceFiles {
		t.Errorf("Source = %q, want %q", inv.Source, inventory.SourceFiles)
	}
	if inv.CollectorSchema != inventory.CurrentCollectorSchema {
		t.Errorf("CollectorSchema = %d, want %d", inv.CollectorSchema, inventory.CurrentCollectorSchema)
	}
}

func TestNewClients(t *testing.T) {
	cfg := &rest.Config{Host: "https://127.0.0.1:6443"}
	c, err := NewClients(cfg)
	if err != nil {
		t.Fatalf("NewClients: %v", err)
	}
	if c.Kube == nil || c.Metadata == nil || c.Discovery == nil || c.RESTClient == nil || c.APIExtensions == nil {
		t.Errorf("NewClients left a nil client: %+v", c)
	}
}

// client-go's default client-side limit (5 QPS, burst 10) would make the
// Helm collector's one GET per release take a minute on a cluster with
// 300 releases. A limit the caller sets is kept.
func TestNewClientsRaisesDefaultRateLimit(t *testing.T) {
	c, err := NewClients(&rest.Config{Host: "https://127.0.0.1:6443"})
	if err != nil {
		t.Fatal(err)
	}
	for name, rc := range map[string]rest.Interface{"core": c.Kube.CoreV1().RESTClient(), "rest": c.RESTClient} {
		if qps := rc.GetRateLimiter().QPS(); qps != clientQPS {
			t.Errorf("%s client QPS = %v, want %v", name, qps, clientQPS)
		}
	}
	cfg := &rest.Config{Host: "https://127.0.0.1:6443", QPS: 7, Burst: 9}
	if c, err = NewClients(cfg); err != nil {
		t.Fatal(err)
	}
	if qps := c.Kube.CoreV1().RESTClient().GetRateLimiter().QPS(); qps != 7 || cfg.QPS != 7 {
		t.Errorf("QPS = %v (caller's cfg %v), want the caller's 7 kept and cfg untouched", qps, cfg.QPS)
	}
}

// recordWarnings stands in for client-go's default warning handler, which
// prints every apiserver Warning header to stderr as a klog line.
type recordWarnings struct{ got *[]string }

func (r recordWarnings) HandleWarningHeaderWithContext(_ context.Context, _ int, _ string, msg string) {
	*r.got = append(*r.got, msg)
}

func TestNewClientsDiscardsAPIWarnings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Warning", `299 - "batch/v1beta1 CronJob is deprecated in v1.21+, unavailable in v1.25+; use batch/v1 CronJob"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"NamespaceList","apiVersion":"v1","metadata":{},"items":[]}`))
	}))
	defer srv.Close()
	var got []string
	rest.SetDefaultWarningHandlerWithContext(recordWarnings{&got})
	defer rest.SetDefaultWarningHandler(rest.WarningLogger{}) // client-go's default

	ctx := context.Background()
	cfg := &rest.Config{Host: srv.URL}
	// Control: a client left on the default handler does see the warning.
	plain, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("control client recorded %d warnings, want 1 (the test server must send one)", len(got))
	}
	got = nil

	c, err := NewClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Metadata.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).List(ctx, metav1.ListOptions{})
	_, _ = c.RESTClient.Get().AbsPath("/metrics").DoRaw(ctx)
	if len(got) != 0 {
		t.Errorf("NewClients clients passed %d warnings to the default (stderr) handler: %q", len(got), got)
	}
	if cfg.WarningHandler != nil || cfg.WarningHandlerWithContext != nil {
		t.Error("NewClients must not modify the caller's rest.Config")
	}
}

// The typed client reads whole Pods, Nodes, Namespaces and Secrets, and the
// byte counts in docs/operations/scale.md are protobuf's (#71): client-go
// asks for it by default, falling back to JSON where the apiserver has none.
// This pins that nothing in NewClients switches it off. /metrics is text and
// keeps its own Accept header.
func TestNewClientsAskForProtobuf(t *testing.T) {
	gotAccept := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept[r.URL.Path] = r.Header.Get("Accept")
		if r.URL.Path != "/api/v1/pods" {
			http.NotFound(w, r)
			return
		}
		list := &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "x"}}, {ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "x"}}}}
		w.Header().Set("Content-Type", "application/vnd.kubernetes.protobuf")
		if err := protobuf.NewSerializer(kubescheme.Scheme, kubescheme.Scheme).Encode(list, w); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	c, err := NewClients(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pods, err := c.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil || len(pods.Items) != 2 || pods.Items[1].Name != "b" {
		t.Fatalf("a protobuf PodList read back as %+v, err %v", pods, err)
	}
	if a := gotAccept["/api/v1/pods"]; !strings.HasPrefix(a, "application/vnd.kubernetes.protobuf") || !strings.Contains(a, "application/json") {
		t.Errorf("pods Accept = %q, want protobuf first and JSON as the fallback", a)
	}
	_, _ = c.RESTClient.Get().AbsPath("/metrics").DoRaw(ctx)
	if a := gotAccept["/metrics"]; strings.Contains(a, "protobuf") {
		t.Errorf("/metrics Accept = %q, want no protobuf: it is a text endpoint", a)
	}
}
