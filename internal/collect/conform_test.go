package collect

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #268: anyone who can create a Pod, a Secret or a ConfigMap in one
// namespace could make the server refuse (422) every push of a cluster for
// ever, because the collectors copied values the server's ingest holds to a
// rule: a Helm release's name (a label value), the names of the objects in
// its stored manifest (text in a payload), an image string (no length
// limit), a chart's kubeVersion. Collect now leaves nothing the server
// refuses: the offending release, object or repository is left out and
// named in its capability, which is partial.

const badManifest = `---
# Source: app/templates/cron.yaml
apiVersion: batch/v1beta1
kind: CronJob
metadata:
  name: Bad/Name
  namespace: tenant
---
# Source: app/templates/ok.yaml
apiVersion: batch/v1beta1
kind: CronJob
metadata:
  name: fine
  namespace: tenant
`

func skippedOf(inv inventory.Inventory, c inventory.Capability) []string {
	return inv.Capabilities[c].Skipped
}

// collectCluster runs Collect against a cluster of the given pods and Helm
// release Secrets.
func collectCluster(t *testing.T, pods []*corev1.Pod, secrets ...*corev1.Secret) inventory.Inventory {
	t.Helper()
	var objs []runtime.Object
	for _, s := range secrets {
		objs = append(objs, s)
	}
	kube, meta := helmClients(t, objs...)
	for _, p := range pods {
		if _, err := kube.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	disc := fakeDiscovery()
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.35.2"}
	c := Clients{Kube: kube, Metadata: meta, Discovery: disc}
	return Collect(context.Background(), c, loadKB(t), Options{})
}

func requireAdmissible(t *testing.T, inv inventory.Inventory) {
	t.Helper()
	if err := inv.Admit(); err != nil {
		t.Fatalf("the collected inventory is refused by the server: %v", err)
	}
}

// (c) A release labelled name=Bad_Name: the label is not the release's name
// (a name is an RFC 1123 subdomain), so the release is recorded under the
// name its object carries, which the apiserver validated.
func TestCollectHelmReleaseLabelThatIsNoNameFallsBackToTheObjectName(t *testing.T) {
	s := helmSecret(t, helmRev{ns: "tenant", release: "shop", rev: 1, status: "deployed", chart: "app", chartVersion: "1.0.0", appVersion: "1.0.0"})
	s.Labels["name"] = "Bad_Name"
	inv := collectCluster(t, nil, s)
	requireAdmissible(t, inv)
	if len(inv.HelmReleases) != 1 || inv.HelmReleases[0].Name != "shop" {
		t.Errorf("releases = %+v, want one named shop, from the object's name", inv.HelmReleases)
	}
}

// (b) A chart whose kubeVersion (or name, or versions) is over the limit is
// not recorded, and named: a cut constraint would be a narrower one, not
// the chart's.
func TestCollectHelmChartMetadataOverTheLimitIsAGapNotARefusedPush(t *testing.T) {
	for name, rev := range map[string]helmRev{
		"kubeVersion":  {kubeVersion: ">=1.0.0-0" + strings.Repeat(" || >=1.0.0-0", 1600)},
		"chart name":   {chart: strings.Repeat("c", 17<<10)},
		"appVersion":   {appVersion: strings.Repeat("1", 17<<10)},
		"chartVersion": {chartVersion: strings.Repeat("1", 17<<10)},
	} {
		t.Run(name, func(t *testing.T) {
			rev.ns, rev.release, rev.rev, rev.status = "tenant", "app", 1, "deployed"
			if rev.chart == "" {
				rev.chart = "app"
			}
			if rev.chartVersion == "" {
				rev.chartVersion = "1.0.0"
			}
			good := helmSecret(t, helmRev{ns: "tenant", release: "fine", rev: 1, status: "deployed", chart: "fine", chartVersion: "1.0.0"})
			inv := collectCluster(t, nil, helmSecret(t, rev), good)
			requireAdmissible(t, inv)
			st := inv.Capabilities[inventory.CapHelm]
			if !st.Available || !st.Partial || !slices.Equal(st.Skipped, []string{"tenant/app"}) || !strings.Contains(st.Reason, "tenant/app: chart metadata has a value over") {
				t.Errorf("helm = %+v, want partial, skipping tenant/app, saying why", st)
			}
			if len(inv.HelmReleases) != 1 || inv.HelmReleases[0].Name != "fine" {
				t.Errorf("releases = %+v, want only the genuine one", inv.HelmReleases)
			}
		})
	}
}

// (d) A manifest object whose name is no object name is left out of the
// release's API usage, counted in objectsOmitted, and the release named:
// its manifest is not assessed whole.
func TestCollectHelmManifestObjectWithAnInvalidNameIsLeftOutAndNamed(t *testing.T) {
	inv := collectCluster(t, nil, helmSecret(t, helmRev{ns: "tenant", release: "app", rev: 1, status: "deployed", chart: "app", chartVersion: "1.0.0", manifest: badManifest}))
	requireAdmissible(t, inv)
	if len(inv.HelmReleases) != 1 || len(inv.HelmReleases[0].ManifestAPIs) != 1 {
		t.Fatalf("releases = %+v, want one with one flagged API", inv.HelmReleases)
	}
	u := inv.HelmReleases[0].ManifestAPIs[0]
	if u.Count != 2 || len(u.Objects) != 1 || u.Objects[0].Name != "fine" || u.ObjectsOmitted != 1 {
		t.Errorf("usage = %+v, want both counted, the genuine one listed, one omitted", u)
	}
	st := inv.Capabilities[inventory.CapHelm]
	if !st.Partial || !slices.Equal(st.Skipped, []string{"tenant/app"}) || !strings.Contains(st.Reason, "object(s) of API usage dropped for a name that is not an object name") {
		t.Errorf("helm = %+v, want partial, skipping tenant/app, saying what was dropped", st)
	}
}

// (a) An image repository over the limit cannot be an add-on's: it is
// counted in unrecognizedImagesOmitted, not listed, and the capability says so.
func TestCollectImageRepositoryOverTheLimitIsCountedNotListed(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "tenant"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.example.com/" + strings.Repeat("a", 17<<10) + ":1"}, {Name: "d", Image: "registry.example.com/team/app:1"}}},
	}
	inv := collectCluster(t, []*corev1.Pod{pod})
	requireAdmissible(t, inv)
	if !slices.Equal(inv.UnrecognizedImages, []string{"registry.example.com/team/app"}) || inv.UnrecognizedImagesOmitted != 1 {
		t.Errorf("unrecognized = %q omitted %d, want the genuine repository, one omitted", inv.UnrecognizedImages, inv.UnrecognizedImagesOmitted)
	}
	if st := inv.Capabilities[inventory.CapAddOns]; !st.Partial || !strings.Contains(st.Reason, "image repositor") {
		t.Errorf("addons = %+v, want partial, saying an image repository was not listed", st)
	}
}

// Every shape at once, through Collect as the agent runs it: the inventory
// is admitted and the genuine data survives. Run in the agent's tests too:
// what is pushed is accepted (202), where it was refused (422) for ever.
func TestCollectAdversarialClusterIsAdmitted(t *testing.T) {
	bad := helmSecret(t, helmRev{ns: "tenant", release: "app", rev: 1, status: "deployed", chart: "app", chartVersion: "1.0.0", manifest: badManifest, kubeVersion: strings.Repeat("x", 20<<10)})
	labelled := helmSecret(t, helmRev{ns: "tenant", release: "lab", rev: 1, status: "deployed", chart: "lab", chartVersion: "1.0.0", manifest: badManifest})
	labelled.Labels["name"] = "Bad_Name"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "tenant"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.example.com/" + strings.Repeat("a", 17<<10) + ":1"}}},
	}
	inv := collectCluster(t, []*corev1.Pod{pod}, bad, labelled)
	requireAdmissible(t, inv)
	if len(inv.HelmReleases) != 1 || inv.HelmReleases[0].Name != "lab" {
		t.Errorf("releases = %+v, want the labelled one under its object's name", inv.HelmReleases)
	}
	for _, c := range []inventory.Capability{inventory.CapHelm, inventory.CapAddOns} {
		if !inv.Capabilities[c].Partial {
			t.Errorf("%s = %+v, want partial", c, inv.Capabilities[c])
		}
	}
	if s := skippedOf(inv, inventory.CapHelm); !slices.Equal(s, []string{"tenant/app", "tenant/lab"}) {
		t.Errorf("helm skipped = %q, want both releases named", s)
	}
}

// A pod's creator chooses its image tag, and an add-on's version is read
// from it: pods that run an add-on's image with a 17 KiB "version" in each
// of many namespaces (an install is per add-on and namespace) made more
// installs than Conform's one-at-a-time net drops, and the agent skipped
// its push on every tick for ever (#268). A tag with no version a release
// could have reads as no version, and the inventory is admitted.
func TestCollectAddOnsWithAHostileImageTagInManyNamespacesAreAdmitted(t *testing.T) {
	hostile := "v1." + strings.Repeat("9", 17<<10)
	images := []string{
		"registry.k8s.io/ingress-nginx/controller:" + hostile,
		"quay.io/jetstack/cert-manager-controller:" + hostile,
	}
	var pods []*corev1.Pod
	for n := range 40 {
		ns := "tenant-" + strconv.Itoa(n)
		for i, img := range images {
			pods = append(pods, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p" + strconv.Itoa(i), Namespace: ns},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: img}}},
			})
		}
	}
	inv := collectCluster(t, pods)
	requireAdmissible(t, inv)
	if len(inv.AddOns) == 0 {
		t.Fatal("no add-on detected: the test must exercise add-on installs")
	}
	for _, a := range inv.AddOns {
		if a.Version != "" {
			t.Errorf("add-on %s version = %d bytes, want none: the tag names no release", a.ID, len(a.Version))
		}
	}
}

// What Collect leaves out so that the server accepts the push is handed to
// Options.OnConform (the agent logs it every tick); a clean cluster calls
// nothing. The notes name no identifier.
func TestCollectReportsWhatConformLeftOut(t *testing.T) {
	collect := func(secrets ...*corev1.Secret) (notes []string, calls int) {
		var objs []runtime.Object
		for _, s := range secrets {
			objs = append(objs, s)
		}
		kube, meta := helmClients(t, objs...)
		disc := fakeDiscovery()
		disc.FakedServerVersion = &version.Info{GitVersion: "v1.35.2"}
		Collect(context.Background(), Clients{Kube: kube, Metadata: meta, Discovery: disc}, loadKB(t),
			Options{OnConform: func(n []string) { notes, calls = n, calls+1 }})
		return notes, calls
	}
	notes, calls := collect(helmSecret(t, helmRev{ns: "tenant", release: "app", rev: 1, status: "deployed", chart: "app", chartVersion: "1.0.0", manifest: badManifest}))
	if calls != 1 || !strings.Contains(strings.Join(notes, "\n"), "object(s) of API usage dropped") {
		t.Errorf("OnConform called %d time(s) with %q, want once, naming what was dropped", calls, notes)
	}
	if strings.Contains(strings.Join(notes, "\n"), "Bad/Name") {
		t.Errorf("notes %q carry the hostile identifier", notes)
	}
	if notes, calls := collect(helmSecret(t, helmRev{ns: "tenant", release: "app", rev: 1, status: "deployed", chart: "app", chartVersion: "1.0.0"})); calls != 0 {
		t.Errorf("a clean cluster called OnConform with %q, want it not called", notes)
	}
}
