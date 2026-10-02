//go:build !windows

// controller-runtime v0.25.2's envtest does not compile for windows
// (pkg/internal/testing/process redeclares signalProcess), and cross-build
// compiles every test for every release platform; this one only ever runs
// on Linux and macOS.

package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// envtestObject is one object the matrix test creates: its resource, the
// namespace ("" = cluster-scoped) and manifest.
type envtestObject struct {
	gvr  schema.GroupVersionResource
	ns   string
	yaml string
}

// envtestGAObjects are GA-only objects, served on every minor of the
// matrix (1.24 to 1.28). A scan of a cluster holding only these has
// nothing to migrate.
var envtestGAObjects = []envtestObject{
	{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "default", `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec: {containers: [{name: web, image: registry.k8s.io/pause:3.9}]}
`},
	{schema.GroupVersionResource{Version: "v1", Resource: "services"}, "default", `
apiVersion: v1
kind: Service
metadata: {name: web}
spec:
  selector: {app: web}
  ports: [{port: 80}]
`},
	{schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, "default", `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: web}
spec:
  rules:
    - host: web.example.com
      http:
        paths:
          - {path: /, pathType: Prefix, backend: {service: {name: web, port: {number: 80}}}}
`},
	{schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, "default", `
apiVersion: batch/v1
kind: CronJob
metadata: {name: nightly}
spec:
  schedule: "0 3 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers: [{name: job, image: registry.k8s.io/pause:3.9}]
`},
	{schema.GroupVersionResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"}, "default", `
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: web}
spec:
  minAvailable: 1
  selector: {matchLabels: {app: web}}
`},
}

// envtestBeta is the deprecated beta API a minor of the matrix still
// serves, and that the test writes one object through. The KB schedules
// its removal at removedIn: the test checks that against the KB, so a
// wrong row fails instead of passing on a stale assumption.
type envtestBeta struct {
	envtestObject
	kind      string
	removedIn inventory.Version
}

var flowSchemaV1beta2 = envtestBeta{envtestObject{
	schema.GroupVersionResource{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2", Resource: "flowschemas"}, "", `
apiVersion: flowcontrol.apiserver.k8s.io/v1beta2
kind: FlowSchema
metadata: {name: legacy}
spec:
  priorityLevelConfiguration: {name: global-default}
  matchingPrecedence: 9000
  distinguisherMethod: {type: ByUser}
  rules:
    - subjects: [{kind: Group, group: {name: "system:unauthenticated"}}]
      nonResourceRules: [{verbs: ["*"], nonResourceURLs: ["*"]}]
`}, "FlowSchema", inventory.Version{Major: 1, Minor: 29}}

// envtestBetas is keyed by the minor of the kube-apiserver.
var envtestBetas = map[int]envtestBeta{
	24: {envtestObject{schema.GroupVersionResource{Group: "batch", Version: "v1beta1", Resource: "cronjobs"}, "default", `
apiVersion: batch/v1beta1
kind: CronJob
metadata: {name: legacy}
spec:
  schedule: "0 4 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers: [{name: job, image: registry.k8s.io/pause:3.9}]
`}, "CronJob", inventory.Version{Major: 1, Minor: 25}},
	25: {envtestObject{schema.GroupVersionResource{Group: "autoscaling", Version: "v2beta2", Resource: "horizontalpodautoscalers"}, "default", `
apiVersion: autoscaling/v2beta2
kind: HorizontalPodAutoscaler
metadata: {name: legacy}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: web}
  minReplicas: 1
  maxReplicas: 3
  metrics:
    - type: Resource
      resource: {name: cpu, target: {type: Utilization, averageUtilization: 80}}
`}, "HorizontalPodAutoscaler", inventory.Version{Major: 1, Minor: 26}},
	26: {envtestObject{schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1beta1", Resource: "csistoragecapacities"}, "default", `
apiVersion: storage.k8s.io/v1beta1
kind: CSIStorageCapacity
metadata: {name: legacy}
storageClassName: standard
`}, "CSIStorageCapacity", inventory.Version{Major: 1, Minor: 27}},
	27: flowSchemaV1beta2,
	28: flowSchemaV1beta2,
}

// TestEnvtestMatrix runs the real collector and engine, the way `scan`
// does, against a real kube-apiserver and etcd (controller-runtime's
// envtest). CI runs it once per Kubernetes minor from 1.24 to 1.28, the
// minors kind's node images do not cover (#135, #69).
//
// There is no kubelet, controller manager or node, so what the
// apiserver alone cannot answer must come back not assessed, which the
// test checks rather than assumes.
//
// Run: make envtest ENVTEST_MINOR=1.24
// or:  KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.24.x) UPGRADESCOPE_ENVTEST=1 go test ./internal/collect -run TestEnvtestMatrix -v
func TestEnvtestMatrix(t *testing.T) {
	if os.Getenv("UPGRADESCOPE_ENVTEST") != "1" {
		t.Skip("envtest matrix: set UPGRADESCOPE_ENVTEST=1 and KUBEBUILDER_ASSETS (make envtest)")
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("UPGRADESCOPE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set; run make envtest, or point it at setup-envtest's kube-apiserver and etcd")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start kube-apiserver and etcd: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop kube-apiserver and etcd: %v", err)
		}
	})

	clients, err := NewClients(cfg)
	if err != nil {
		t.Fatalf("build clients: %v", err)
	}
	sv, err := clients.Discovery.ServerVersion()
	if err != nil {
		t.Fatalf("server version: %v", err)
	}
	server, err := inventory.ParseVersion(sv.GitVersion)
	if err != nil {
		t.Fatalf("parse server version %q: %v", sv.GitVersion, err)
	}
	t.Logf("kube-apiserver %s", sv.GitVersion)
	// The job names the minor it means to test: assets for another one
	// (a stale cache, a wrong pin) must not pass as this minor.
	if want := os.Getenv("UPGRADESCOPE_ENVTEST_MINOR"); want != "" && want != server.String() {
		t.Fatalf("UPGRADESCOPE_ENVTEST_MINOR=%s but KUBEBUILDER_ASSETS holds a %s kube-apiserver", want, server)
	}
	beta, ok := envtestBetas[server.Minor]
	if server.Major != 1 || !ok {
		t.Fatalf("kube-apiserver %s is outside the matrix (1.24 to 1.28): add its deprecated beta API to envtestBetas", server)
	}

	k, err := kb.Load()
	if err != nil {
		t.Fatalf("load knowledge base: %v", err)
	}
	// The beta writes below draw deprecation warnings; they are the point.
	dynCfg := rest.CopyConfig(cfg)
	dynCfg.WarningHandlerWithContext = rest.NoWarnings{}
	dyn, err := dynamic.NewForConfig(dynCfg)
	if err != nil {
		t.Fatalf("build dynamic client: %v", err)
	}

	// scan is runScan's live path: collect, then evaluate at a target.
	// Every scan of the test is judged at the same instant.
	now := time.Now()
	scan := func(t *testing.T, target inventory.Version) (inventory.Inventory, engine.Report) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		clients, err := NewClients(cfg)
		if err != nil {
			t.Fatalf("build clients: %v", err)
		}
		inv := Collect(ctx, clients, k, Options{})
		return inv, engine.Evaluate(inv, k, target, now)
	}

	next := server.Next()
	for _, o := range envtestGAObjects {
		envtestCreate(t, dyn, o)
	}

	t.Run("GA-only objects have no removed-API finding or other blocker", func(t *testing.T) {
		inv, report := scan(t, next)
		for _, f := range report.Findings {
			switch {
			case f.Category == engine.CatRemovedAPI:
				t.Errorf("removed-api finding on %s for a cluster holding only GA objects: %s: %s", server, f.Title, f.Detail)
			case f.Severity != engine.SevInfo:
				// Nothing else is wrong either: a finding here is the
				// scanner's own requests read as a caller (#123).
				t.Errorf("%s finding on a fresh %s cluster holding only GA objects: %s: %s", f.Severity, server, f.Title, f.Detail)
			}
		}
		if report.ServerVersion != sv.GitVersion {
			t.Errorf("report.ServerVersion = %q, want %q", report.ServerVersion, sv.GitVersion)
		}
		// What the apiserver alone can answer was answered: the versions
		// and the API usage. Whatever else came back unavailable or
		// partial has to be in the report's not-assessed list with the
		// collector's reason, never read as clean.
		for _, c := range []inventory.Capability{inventory.CapVersions, inventory.CapAPIUsage} {
			if st := inv.Capabilities[c]; !st.Available {
				t.Errorf("capability %s unavailable on a bare apiserver: %s", c, st.Reason)
			}
		}
		for c, st := range inv.Capabilities {
			if st.Available && !st.Partial {
				continue
			}
			i := slices.IndexFunc(report.NotAssessed, func(g engine.CapabilityGap) bool { return g.Capability == c })
			if i < 0 {
				t.Errorf("capability %s is %+v but the report does not list it as not assessed (verdict %s)", c, st, report.Verdict)
				continue
			}
			if g := report.NotAssessed[i]; g.Reason != st.Reason || g.Partial != st.Partial {
				t.Errorf("not assessed %s = %+v, want the collector's status %+v", c, g, st)
			}
		}
		t.Logf("verdict %s, score %d, %d finding(s); capabilities: %s; notAssessed %+v", report.Verdict, report.Score, len(report.Findings), capabilitySummary(inv), report.NotAssessed)
	})

	t.Run("a second scan of an unchanged cluster is identical", func(t *testing.T) {
		inv1, r1 := scan(t, next)
		inv2, r2 := scan(t, next)
		// The scanner's own requests (its LISTs at a deprecated version,
		// the /metrics scrape) must not change what the next scan sees
		// (#123, #139).
		if a, b := indentJSON(t, r1), indentJSON(t, r2); !bytes.Equal(a, b) {
			t.Errorf("reports differ between two scans of an unchanged %s cluster\nfirst:\n%s\nsecond:\n%s", server, a, b)
		}
		inv1.CollectedAt, inv2.CollectedAt = time.Time{}, time.Time{}
		if a, b := indentJSON(t, inv1), indentJSON(t, inv2); !bytes.Equal(a, b) {
			t.Errorf("inventories differ between two scans of an unchanged %s cluster\nfirst:\n%s\nsecond:\n%s", server, a, b)
		}
	})

	t.Run("an object written through a deprecated beta API blocks at its removal", func(t *testing.T) {
		gv := beta.gvr.GroupVersion()
		e, known := kb.NewIndex(k.APILifecycle).Lookup(gv.Group, gv.Version, beta.kind)
		if !known || e.Removed == nil || *e.Removed != beta.removedIn {
			t.Fatalf("KB entry for %s %s = %+v (known %v), want removal in %s", gv, beta.kind, e, known, beta.removedIn)
		}
		if !envtestServes(clients, beta.gvr) {
			t.Fatalf("kube-apiserver %s does not serve %s, which envtestBetas says it does", server, beta.gvr)
		}
		envtestCreate(t, dyn, beta.envtestObject)

		inv, report := scan(t, beta.removedIn)
		// The apiserver's own count of deprecated requests names the write
		// above: its metric and labels still parse on this minor.
		wantCall := inventory.DeprecatedCall{Group: gv.Group, Version: gv.Version, Resource: beta.gvr.Resource, RemovedRelease: beta.removedIn.String()}
		if !slices.Contains(inv.DeprecatedCalls, wantCall) {
			t.Errorf("deprecated-calls has no row %+v for the write through %s; rows: %+v", wantCall, gv, inv.DeprecatedCalls)
		}
		key := fmt.Sprintf("removed-api/%s/%s/%s", gv.Group, gv.Version, beta.kind)
		var got *engine.Finding
		for i, f := range report.Findings {
			switch {
			case f.Key == key:
				got = &report.Findings[i]
			case f.Category == engine.CatRemovedAPI:
				t.Errorf("unexpected removed-api finding %s: only %s was written through a beta API", f.Key, beta.kind)
			}
		}
		if got == nil {
			t.Fatalf("no %s finding at target %s; findings: %s", key, beta.removedIn, findingKeys(report))
		}
		if got.Severity != engine.SevBlocker {
			t.Errorf("%s at target %s: severity %s, want blocker", key, beta.removedIn, got.Severity)
		}
		name := envtestUnstructured(beta.yaml).GetName()
		if len(got.Objects) != 1 || got.Objects[0].Name != name {
			t.Errorf("%s objects = %+v, want only %q", key, got.Objects, name)
		}
		if report.Verdict != engine.VerdictBlocked || report.Ready {
			t.Errorf("verdict = %s, ready = %v, want blocked", report.Verdict, report.Ready)
		}

		// One minor earlier the removal is still ahead: a warning.
		earlier := inventory.Version{Major: beta.removedIn.Major, Minor: beta.removedIn.Minor - 1}
		if earlier.Compare(server) <= 0 {
			return // not an upgrade of this cluster: nothing to judge
		}
		_, report = scan(t, earlier)
		i := slices.IndexFunc(report.Findings, func(f engine.Finding) bool { return f.Key == key })
		if i < 0 || report.Findings[i].Severity != engine.SevWarning {
			t.Errorf("%s at target %s: want a warning, findings: %s", key, earlier, findingKeys(report))
		}
	})
}

func envtestUnstructured(manifest string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), &u.Object); err != nil {
		panic(fmt.Sprintf("envtest manifest: %v\n%s", err, manifest))
	}
	return u
}

// envtestCreate creates o through the version its manifest names.
func envtestCreate(t *testing.T, dyn dynamic.Interface, o envtestObject) {
	t.Helper()
	u := envtestUnstructured(o.yaml)
	var ri dynamic.ResourceInterface = dyn.Resource(o.gvr)
	if o.ns != "" {
		ri = dyn.Resource(o.gvr).Namespace(o.ns)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := ri.Create(ctx, u, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create %s %s through %s: %v", u.GetKind(), u.GetName(), o.gvr.GroupVersion(), err)
	}
}

// envtestServes reports whether discovery lists gvr.
func envtestServes(c Clients, gvr schema.GroupVersionResource) bool {
	list, err := c.Discovery.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		return false
	}
	return slices.ContainsFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Name == gvr.Resource })
}

func indentJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func capabilitySummary(inv inventory.Inventory) string {
	var parts []string
	for c, st := range inv.Capabilities {
		s := "available"
		switch {
		case !st.Available:
			s = "unavailable"
		case st.Partial:
			s = "partial"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", c, s))
	}
	slices.Sort(parts)
	return strings.Join(parts, " ")
}

func findingKeys(r engine.Report) string {
	var keys []string
	for _, f := range r.Findings {
		keys = append(keys, f.Key)
	}
	return strings.Join(keys, ", ")
}
