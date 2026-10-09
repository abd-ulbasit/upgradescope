package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// eolIngressNginx is a Deployment running an ingress-nginx controller,
// end of life in the embedded registry.
const eolIngressNginx = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ingress-nginx-controller
  namespace: ingress-nginx
spec:
  template:
    spec:
      containers:
        - name: controller
          image: registry.k8s.io/ingress-nginx/controller:v1.8.1
`

// findingOf returns the finding with key in a gate answer.
func findingOf(b failOnGateResponse, key string) (severity, source string, ok bool) {
	for _, f := range b.Findings {
		if f.Key == key {
			return f.Severity, f.Source, true
		}
	}
	return "", "", false
}

// #150: with ?cluster=, an add-on the manifests deploy is judged inside
// the cluster: an EOL ingress-nginx posted against a clean stored cluster
// is the manifests' blocker, the finding the gate gives without ?cluster=.
func TestGateClusterManifestAddOn(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.KB = k }).Handler())
	defer ts.Close()
	// An agent that collected with this server's knowledge base (the gate
	// judges a cluster collected with another as unknown: #268).
	seed := map[string]any{}
	if err := json.Unmarshal(pushReqBody(t, testInventory()), &seed); err != nil {
		t.Fatal(err)
	}
	seed["kbVersion"] = k.Version
	body, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if resp, out := postSnapshot(t, ts, "ingest-tok", body, false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
	resp, raw := postGate(t, ts, "?target=1.35&cluster=prod-eu-1", "", deploymentManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stored cluster is not clean: %d %s", resp.StatusCode, raw)
	}

	for _, q := range []string{"?target=1.35&cluster=prod-eu-1", "?target=1.35"} {
		resp, raw := postGate(t, ts, q, "", eolIngressNginx, "application/x-yaml")
		b := decodeGate(t, raw)
		sev, src, ok := findingOf(b, "eol-addon/ingress-nginx")
		if resp.StatusCode != http.StatusUnprocessableEntity || b.Verdict != "blocked" || !ok || sev != "blocker" || src != "manifest" {
			t.Errorf("%s: %d verdict %q eol-addon/ingress-nginx %q from %q (found %v), want 422 blocked by the manifests' blocker\n%s",
				q, resp.StatusCode, b.Verdict, sev, src, ok, raw)
		}
	}
}

// widgetCRD is a cluster CRD for example.com Widgets that serves v1 and
// no longer serves v1alpha1.
func widgetCRD() inventory.CRD {
	return inventory.CRD{
		Group: "example.com", Kind: "Widget", Plural: "widgets",
		Versions: []inventory.CRDVersion{
			{Name: "v1alpha1", Served: false},
			{Name: "v1", Served: true, Storage: true},
		},
		StoredVersions: []string{"v1"},
	}
}

func widget(version, name string) string {
	return "apiVersion: example.com/" + version + "\nkind: Widget\nmetadata: {name: " + name + ", namespace: shop}\n"
}

// widgetCluster serves a gate whose cluster prod-eu-1 has the Widget CRD,
// adjusted by edit.
func widgetCluster(t *testing.T, edit func(*inventory.CRD)) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	t.Cleanup(ts.Close)
	inv := testInventory()
	crd := widgetCRD()
	if edit != nil {
		edit(&crd)
	}
	inv.CRDs = []inventory.CRD{crd}
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
	return ts
}

const unservedWidget = "crd-version/unserved/example.com/v1alpha1/Widget"

// #150: with ?cluster=, posted custom resources are judged against the
// cluster's CRDs: one at a version the cluster's CRD does not serve is
// the manifests' crd-version blocker, as it is without ?cluster= when the
// CRD is posted with it.
func TestGateClusterManifestCustomResource(t *testing.T) {
	ts := widgetCluster(t, nil)

	resp, raw := postGate(t, ts, "?target=1.35&cluster=prod-eu-1", "", widget("v1", "ok"), "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a Widget at the served v1: %d, want 200\n%s", resp.StatusCode, raw)
	}
	resp, raw = postGate(t, ts, "?target=1.35&cluster=prod-eu-1", "", widget("v1alpha1", "old"), "application/x-yaml")
	b := decodeGate(t, raw)
	if sev, src, ok := findingOf(b, unservedWidget); resp.StatusCode != http.StatusUnprocessableEntity || !ok || sev != "blocker" || src != "manifest" {
		t.Errorf("a Widget at the unserved v1alpha1: %d %s %q from %q (found %v), want 422 with the manifests' blocker\n%s",
			resp.StatusCode, unservedWidget, sev, src, ok, raw)
	}
}

// #150: a CRD in the manifests replaces the cluster's, as kubectl apply
// would, and the posted custom resources are judged against it.
func TestGateClusterManifestCRDReplacesClusters(t *testing.T) {
	ts := widgetCluster(t, func(c *inventory.CRD) { c.Versions[0].Served = true })
	crd := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: widgets.example.com}
spec:
  group: example.com
  scope: Namespaced
  names: {kind: Widget, plural: widgets}
  versions:
    - {name: v1alpha1, served: false, storage: false}
    - {name: v1, served: true, storage: true}
---
`
	resp, raw := postGate(t, ts, "?target=1.35&cluster=prod-eu-1", "", widget("v1alpha1", "old"), "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the cluster's CRD serves v1alpha1: %d, want 200\n%s", resp.StatusCode, raw)
	}
	// Without ?cluster= the same stream gives the same finding, judged
	// against the posted CRD alone (scan --files parity).
	for _, q := range []string{"?target=1.35&cluster=prod-eu-1", "?target=1.35"} {
		resp, raw = postGate(t, ts, q, "", crd+widget("v1alpha1", "old"), "application/x-yaml")
		b := decodeGate(t, raw)
		if sev, src, ok := findingOf(b, unservedWidget); resp.StatusCode != http.StatusUnprocessableEntity || !ok || sev != "blocker" || src != "manifest" {
			t.Errorf("%s: the posted CRD stops serving v1alpha1: %d, want 422 with the manifests' %s blocker\n%s", q, resp.StatusCode, unservedWidget, raw)
		}
	}
}

// upsertAddOns: an install the manifests carry replaces the cluster's
// install of that add-on in the same namespace (the manifests are what
// will run there) and is added otherwise; the cluster's slice is not
// modified.
func TestUpsertAddOns(t *testing.T) {
	cluster := []inventory.AddOnInstance{
		{ID: "cert-manager", Version: "1.11.0", Namespaces: []string{"cert-manager"}, Source: "image"},
		{ID: "ingress-nginx", Namespaces: nil, Source: "ingressclass"},
	}
	manifests := []inventory.AddOnInstance{
		{ID: "cert-manager", Version: "1.16.0", Namespaces: []string{"cert-manager"}, Source: "image"},
		{ID: "cert-manager", Version: "1.16.0", Namespaces: []string{"tenant-a"}, Source: "image"},
	}
	got := upsertAddOns(cluster, manifests)
	want := []inventory.AddOnInstance{manifests[0], cluster[1], manifests[1]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upsertAddOns = %+v\nwant %+v", got, want)
	}
	if cluster[0].Version != "1.11.0" {
		t.Error("the cluster's add-ons must not be modified")
	}

	// A namespace can hold two installs of an add-on (a Helm release and
	// an image on another release line, #165): the manifests replace both.
	cluster = []inventory.AddOnInstance{
		{ID: "istio", Version: "1.28.10", Namespaces: []string{"istio-system"}, Source: "image"},
		{ID: "istio", Version: "1.31.1", ChartVersion: "1.31.1", Namespaces: []string{"istio-system"}, Source: "chart"},
		{ID: "istio", Version: "1.30.5", Namespaces: []string{"mesh"}, Source: "image"},
	}
	manifests = []inventory.AddOnInstance{{ID: "istio", Version: "1.32.0", Namespaces: []string{"istio-system"}, Source: "image"}}
	got = upsertAddOns(cluster, manifests)
	want = []inventory.AddOnInstance{manifests[0], cluster[2]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upsertAddOns = %+v\nwant %+v", got, want)
	}
}

// #150: what the cluster already has stays the cluster's: live custom
// resources at an unserved version do not fail a harmless PR, and a posted
// one at that version is still the PR's, as for removed APIs.
func TestGateClusterResidentCustomResources(t *testing.T) {
	ts := widgetCluster(t, func(c *inventory.CRD) {
		c.Usage = []inventory.APIUsage{{Group: "example.com", Version: "v1alpha1", Kind: "Widget", Count: 1,
			Namespaces: map[string]int{"shop": 1}, Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "legacy", Manager: "old-operator"}}}}
	})
	resp, raw := postGate(t, ts, "?target=1.35&cluster=prod-eu-1", "", deploymentManifest, "application/x-yaml")
	b := decodeGate(t, raw)
	if _, src, ok := findingOf(b, unservedWidget); resp.StatusCode != http.StatusOK || b.ClusterVerdict != "blocked" || !ok || src != "cluster" {
		t.Errorf("harmless PR: %d clusterVerdict %q %s from %q, want 200 with the cluster's blocker\n%s", resp.StatusCode, b.ClusterVerdict, unservedWidget, src, raw)
	}
	resp, raw = postGate(t, ts, "?target=1.35&cluster=prod-eu-1", "", widget("v1alpha1", "new"), "application/x-yaml")
	b = decodeGate(t, raw)
	if _, src, ok := findingOf(b, unservedWidget); resp.StatusCode != http.StatusUnprocessableEntity || !ok || src != "manifest" {
		t.Errorf("a new Widget at v1alpha1: %d %s from %q, want 422 with the manifests' blocker\n%s", resp.StatusCode, unservedWidget, src, raw)
	}
	if !strings.Contains(string(raw), "(2 objects)") {
		t.Errorf("the finding should count the live and the posted Widget:\n%s", raw)
	}
}

// mergeManifests keeps what the cluster knows that a manifest cannot say
// (status.storedVersions), and names custom resources neither the cluster
// nor the manifests define a CRD for.
func TestMergeManifestsCRDs(t *testing.T) {
	cluster := testInventory()
	cluster.CRDs = []inventory.CRD{widgetCRD()}
	cluster.CRDs[0].StoredVersions = []string{"v1alpha1", "v1"}
	manifests := inventory.Inventory{
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{inventory.CapCRDs: {Available: true}},
		APIUsage: []inventory.APIUsage{
			{Group: "example.com", Version: "v1", Kind: "Widget", Count: 1},
			{Group: "other.example.com", Version: "v1", Kind: "Gadget", Count: 1},
		},
		CRDs: []inventory.CRD{{Group: "example.com", Kind: "Widget", Plural: "widgets",
			Versions: []inventory.CRDVersion{{Name: "v1", Served: true, Storage: true}}}},
	}
	proposed := cluster
	mergeManifests(&proposed, manifests)

	if len(proposed.CRDs) != 1 || len(proposed.CRDs[0].Versions) != 1 || !slices.Equal(proposed.CRDs[0].StoredVersions, []string{"v1alpha1", "v1"}) {
		t.Errorf("CRDs = %+v, want the posted Widget CRD with the cluster's storedVersions", proposed.CRDs)
	}
	st := proposed.Capabilities[inventory.CapCRDs]
	if !st.Available || !st.Partial || !slices.Equal(st.Skipped, []string{"other.example.com/v1 Gadget"}) || !strings.Contains(st.Reason, "cluster") {
		t.Errorf("crds = %+v, want partial, naming the Gadget", st)
	}
	if len(cluster.CRDs[0].StoredVersions) != 2 || cluster.Capabilities[inventory.CapCRDs].Partial {
		t.Error("the cluster's inventory must not be modified")
	}
}
