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

// #301 end to end: a rendered Deployment whose image is pinned by digest
// (or ":latest") names its version in its pod labels, which the scan reads,
// so cert-manager 1.12, end of life, is a blocker rather than an
// "addon-no-data" info with a passing verdict.
func TestCollectFilesImageWithoutVersionTakesLabelVersion(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	const digest = "quay.io/jetstack/cert-manager-controller@sha256:3b1ab0b56f1c2f1f9ba0c6a4b9b4b1b4f0b9d2b2b6c9e2e1d0c1b2a3f4e5d6c7"
	for name, image := range map[string]string{"digest": digest, "latest": "quay.io/jetstack/cert-manager-controller:latest"} {
		t.Run(name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"cm.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: cm, namespace: cert-manager}\nspec:\n  selector: {matchLabels: {app: cm}}\n  template:\n    metadata: {labels: {app: cm, app.kubernetes.io/name: cert-manager, app.kubernetes.io/version: v1.12.3, helm.sh/chart: cert-manager-v1.12.3}}\n    spec:\n      containers:\n      - {name: c, image: \"" + image + "\"}\n"})
			inv, _, err := CollectFiles(dir, k)
			if err != nil {
				t.Fatal(err)
			}
			want := []inventory.AddOnInstance{{ID: "cert-manager", Version: "1.12.3", Namespaces: []string{"cert-manager"}, Source: "image"}}
			if !reflect.DeepEqual(inv.AddOns, want) {
				t.Fatalf("addons = %+v, want %+v", inv.AddOns, want)
			}
			rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 33}, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
			i := slices.IndexFunc(rep.Findings, func(f engine.Finding) bool { return f.Key == "eol-addon/cert-manager/1.12" })
			if i < 0 || rep.Findings[i].Severity != engine.SevBlocker || rep.Verdict != engine.VerdictBlocked {
				t.Errorf("verdict %s, findings %+v; want the cert-manager 1.12 end-of-life blocker", rep.Verdict, rep.Findings)
			}
			if slices.ContainsFunc(rep.Findings, func(f engine.Finding) bool { return f.Category == engine.CatAddOnNoData }) {
				t.Errorf("findings %+v, want no addon-no-data", rep.Findings)
			}
		})
	}
}

// A Flux component image on a release line the registry does not map yet
// gives no version, whatever its pod's version label says (a component
// image's line is never guessed from a label), and a digest-only image
// whose label names a different add-on gives none either. The no-data
// detail must not claim the image has no version tag when it has one
// (#301): it says no version was read and names both reasons.
func TestCollectFilesUnreadableImageVersionDetailIsTrue(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	const digest = "ghcr.io/fluxcd/source-controller@sha256:3b1ab0b56f1c2f1f9ba0c6a4b9b4b1b4f0b9d2b2b6c9e2e1d0c1b2a3f4e5d6c7"
	for name, image := range map[string]string{"tagged on an unmapped line": "ghcr.io/fluxcd/source-controller:v1.99.0", "digest only": digest} {
		t.Run(name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"sc.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: source-controller, namespace: flux-system}\nspec:\n  selector: {matchLabels: {app: sc}}\n  template:\n    metadata: {labels: {app: sc, app.kubernetes.io/name: flux, app.kubernetes.io/version: v2.7.0}}\n    spec:\n      containers:\n      - {name: c, image: \"" + image + "\"}\n"})
			inv, _, err := CollectFiles(dir, k)
			if err != nil {
				t.Fatal(err)
			}
			rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 33}, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
			i := slices.IndexFunc(rep.Findings, func(f engine.Finding) bool { return f.Key == "addon-no-data/flux" })
			if i < 0 {
				t.Fatalf("addons %+v, findings %+v; want an addon-no-data finding for flux", inv.AddOns, rep.Findings)
			}
			d := rep.Findings[i].Detail
			for _, want := range []string{"No version was read from the image", "a component image whose release line the registry does not map yet"} {
				if !strings.Contains(d, want) {
					t.Errorf("detail %q lacks %q", d, want)
				}
			}
			for _, bad := range []string{"The image has no version tag", "was not consulted", "label was unreadable"} {
				if strings.Contains(d, bad) {
					t.Errorf("detail %q claims %q", d, bad)
				}
			}
		})
	}
}
