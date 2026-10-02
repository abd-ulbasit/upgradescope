package collect

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// podSpec is a pod spec body with an init container and a container,
// indented to sit under a "spec:" key at the given depth.
func podSpec(indent string) string {
	return strings.ReplaceAll(`spec:
  initContainers:
    - name: init
      image: busybox:1.36
  containers:
    - name: app
      image: registry.k8s.io/ingress-nginx/controller:v1.8.1
`, "\n", "\n"+indent)
}

// #47: files mode reads the images of every pod template, init containers
// included, wherever the workload kind keeps it, and the template's labels.
func TestManifestPodTemplates(t *testing.T) {
	template := func(indent string) string {
		return "template:\n" + indent + "  metadata:\n" + indent + "    labels: {app.kubernetes.io/name: ingress-nginx}\n" + indent + "  " + podSpec(indent+"  ")
	}
	cases := []struct {
		kind, manifest string
	}{
		{"Deployment", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d, namespace: edge}\nspec:\n  " + template("  ")},
		{"DaemonSet", "apiVersion: apps/v1\nkind: DaemonSet\nmetadata: {name: d, namespace: edge}\nspec:\n  " + template("  ")},
		{"StatefulSet", "apiVersion: apps/v1\nkind: StatefulSet\nmetadata: {name: d, namespace: edge}\nspec:\n  " + template("  ")},
		{"ReplicaSet", "apiVersion: apps/v1\nkind: ReplicaSet\nmetadata: {name: d, namespace: edge}\nspec:\n  " + template("  ")},
		{"extensions Deployment", "apiVersion: extensions/v1beta1\nkind: Deployment\nmetadata: {name: d, namespace: edge}\nspec:\n  " + template("  ")},
		{"Job", "apiVersion: batch/v1\nkind: Job\nmetadata: {name: d, namespace: edge}\nspec:\n  " + template("  ")},
		{"CronJob", "apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: d, namespace: edge}\nspec:\n  schedule: '@daily'\n  jobTemplate:\n    spec:\n      " + template("      ")},
		{"Pod", "apiVersion: v1\nkind: Pod\nmetadata:\n  name: d\n  namespace: edge\n  labels: {app.kubernetes.io/name: ingress-nginx}\n" + podSpec("")},
		{"List item", "apiVersion: v1\nkind: List\nitems:\n  - apiVersion: apps/v1\n    kind: Deployment\n    metadata: {name: d, namespace: edge}\n    spec:\n      " + template("      ")},
		{"JSON Deployment", `{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "d", "namespace": "edge"}, "spec": {"template": {
			"metadata": {"labels": {"app.kubernetes.io/name": "ingress-nginx"}},
			"spec": {"initContainers": [{"name": "init", "image": "busybox:1.36"}],
			         "containers": [{"name": "app", "image": "registry.k8s.io/ingress-nginx/controller:v1.8.1"}]}}}}`},
	}
	images := []string{"busybox:1.36", "registry.k8s.io/ingress-nginx/controller:v1.8.1"}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			objs, ev, bad, err := parseManifestStream(strings.NewReader(tc.manifest))
			if err != nil || len(bad) > 0 || len(objs) != 1 {
				t.Fatalf("parse: %d objects, bad %+v, err %v", len(objs), bad, err)
			}
			want := addOnEvidence{
				images:   []nsImage{{"edge", images[0]}, {"edge", images[1]}},
				labelled: []labelledPod{{Namespace: "edge", Labels: appLabels{name: "ingress-nginx"}, Images: images}},
			}
			if !reflect.DeepEqual(ev, want) {
				t.Errorf("evidence = %+v\nwant       %+v", ev, want)
			}
		})
	}
}

func TestManifestAddOnEvidenceOnlyFromWorkloads(t *testing.T) {
	stream := `apiVersion: example.com/v1
kind: Deployment
metadata: {name: not-apps}
spec:
  template:
    spec:
      containers: [{name: c, image: registry.k8s.io/ingress-nginx/controller:v1.8.1}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: cm}
data: {image: registry.k8s.io/ingress-nginx/controller:v1.8.1}
---
apiVersion: networking.k8s.io/v1
kind: IngressClass
metadata: {name: nginx}
spec: {controller: k8s.io/ingress-nginx}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: unrendered-image}
spec:
  template:
    spec:
      containers: [{name: c, image: ""}, {name: d}]
`
	_, ev, bad, err := parseManifestStream(strings.NewReader(stream))
	if err != nil || len(bad) > 0 {
		t.Fatalf("bad %+v, err %v", bad, err)
	}
	want := addOnEvidence{ingressControllers: []string{"k8s.io/ingress-nginx"}}
	if !reflect.DeepEqual(ev, want) {
		t.Errorf("evidence = %+v, want only the IngressClass controller", ev)
	}
}

// #47 end to end against the embedded registry: a rendered Deployment
// running ingress-nginx controller v1.8.1 gives the EOL blocker in files
// mode, and add-ons are assessed; versions and Helm releases stay not
// assessed (a manifest carries neither).
func TestCollectFilesAssessesAddOns(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	dir := writeTree(t, map[string]string{"rendered/controller.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: ingress-nginx-controller\n  namespace: ingress-nginx\nspec:\n  template:\n    " + podSpec("    ")})
	inv, _, err := CollectFiles(dir, k)
	if err != nil {
		t.Fatal(err)
	}
	if st := inv.Capabilities[inventory.CapAddOns]; !st.Available {
		t.Errorf("addons capability = %+v, want available", st)
	}
	want := []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.8.1", Namespaces: []string{"ingress-nginx"}, Source: "image"}}
	if !reflect.DeepEqual(inv.AddOns, want) {
		t.Errorf("addons = %+v, want %+v", inv.AddOns, want)
	}
	if !reflect.DeepEqual(inv.UnrecognizedImages, []string{"docker.io/library/busybox"}) {
		t.Errorf("unrecognized = %v, want the init container's busybox", inv.UnrecognizedImages)
	}

	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 36}, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if rep.Verdict != engine.VerdictBlocked || !slices.ContainsFunc(rep.Findings, func(f engine.Finding) bool { return f.Key == "eol-addon/ingress-nginx" }) {
		t.Errorf("verdict %s, findings %+v; want the ingress-nginx EOL blocker", rep.Verdict, rep.Findings)
	}
	var gaps []inventory.Capability
	for _, g := range rep.NotAssessed {
		gaps = append(gaps, g.Capability)
	}
	if want := []inventory.Capability{inventory.CapDeprecatedCalls, inventory.CapHelm, inventory.CapVersions}; !reflect.DeepEqual(gaps, want) {
		t.Errorf("not assessed = %v, want %v", gaps, want)
	}
}

// An object without metadata.namespace (typical helm template output) is
// one install whose namespace is unset, not a cluster-scoped one.
func TestCollectFilesAddOnNamespaceUnset(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	dir := writeTree(t, map[string]string{"pod.yaml": "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\n" + podSpec("")})
	inv, _, err := CollectFiles(dir, kb.KB{AddOns: addons})
	if err != nil {
		t.Fatal(err)
	}
	if want := []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.8.1", Namespaces: []string{""}, Source: "image"}}; !reflect.DeepEqual(inv.AddOns, want) {
		t.Errorf("addons = %#v, want %#v", inv.AddOns, want)
	}
}
