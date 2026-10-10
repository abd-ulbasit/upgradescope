package collect

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// #228: the pods outside kube-system are listed every Nth collection, and
// the add-ons are detected from the last full pass's images and labels in
// between.

// passClock is a controllable clock for a PodPassCache.
type passClock struct{ t time.Time }

func (c *passClock) now() time.Time { return c.t }

func newPassCache(every int, maxAge time.Duration) (*PodPassCache, *passClock) {
	clk := &passClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	c := NewPodPassCache(every, maxAge)
	c.now = clk.now
	return c, clk
}

// allNamespaceLists counts the lists of the pods outside kube-system, and
// kubeSystemLists those of kube-system alone.
func (s *podServer) allNamespaceLists() (n int) {
	for _, c := range s.calls {
		if c.namespace == "" && c.cont == "" {
			n++
		}
	}
	return n
}

func (s *podServer) kubeSystemLists() (n int) {
	for _, c := range s.calls {
		if c.namespace == "kube-system" && c.cont == "" {
			n++
		}
	}
	return n
}

func addOnVersions(inv inventory.Inventory) map[string]string {
	out := map[string]string{}
	for _, a := range inv.AddOns {
		out[a.ID] = a.Version
	}
	return out
}

// A collection between full passes lists the kube-system pods, which
// versions needs, and not the others; the add-ons are those of a full
// collection of the same cluster, and the age of the reused pass is left
// in the inventory.
func TestPodPassReusedBetweenFullPasses(t *testing.T) {
	cs, disc := podFixture()
	pods := append(tickPods(), appPod("nginx-2", "registry.k8s.io/ingress-nginx/controller:v1.9.4"), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "istiod-1", Namespace: "istio-system",
			Labels: map[string]string{"app.kubernetes.io/name": "istiod", "app.kubernetes.io/version": "1.20.1"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.com/mirror/pilot:custom"}}},
	})
	srv := servePods(cs, pods...)
	k := loadKB(t)
	pass, clk := newPassCache(3, time.Hour)
	clients := Clients{Kube: cs, Discovery: disc}

	full := Collect(context.Background(), clients, k, Options{})
	srv.calls = nil

	for tick := 1; tick <= 7; tick++ {
		srv.calls = nil
		inv := Collect(context.Background(), clients, k, Options{PodPass: pass})
		passTick := tick%3 == 1 // ticks 1, 4, 7
		if got, want := srv.allNamespaceLists(), map[bool]int{true: 1, false: 0}[passTick]; got != want {
			t.Errorf("tick %d: %d lists of the other namespaces, want %d (requests %+v)", tick, got, want, srv.calls)
		}
		if got := srv.kubeSystemLists(); got != 1 {
			t.Errorf("tick %d: %d lists of kube-system, want 1: versions reads them every tick", tick, got)
		}
		if passTick && inv.AddOnEvidenceAgeSeconds != 0 {
			t.Errorf("tick %d: a full pass reports age %d, want none", tick, inv.AddOnEvidenceAgeSeconds)
		}
		if !passTick && inv.AddOnEvidenceAgeSeconds != int64((time.Duration(tick-1)%3)*10*time.Minute/time.Second) {
			t.Errorf("tick %d: age = %ds, want %ds", tick, inv.AddOnEvidenceAgeSeconds, int64((time.Duration(tick-1)%3)*10*time.Minute/time.Second))
		}
		if !reflect.DeepEqual(inv.AddOns, full.AddOns) {
			t.Errorf("tick %d: add-ons = %+v, want those of a full collection %+v", tick, inv.AddOns, full.AddOns)
		}
		if !reflect.DeepEqual(inv.UnrecognizedImages, full.UnrecognizedImages) {
			t.Errorf("tick %d: unrecognized images = %v, want %v", tick, inv.UnrecognizedImages, full.UnrecognizedImages)
		}
		if st := inv.Capabilities[inventory.CapAddOns]; !st.Available || st.Partial || st.Reason != "" {
			t.Errorf("tick %d: addons capability = %+v, want available, complete, no reason: a reused pass is no gap", tick, st)
		}
		clk.t = clk.t.Add(10 * time.Minute)
	}
}

// An add-on upgraded right after a full pass is reported with its old
// version for the next N-1 collections, then with the new one.
func TestPodPassStalenessIsBoundedByTheCount(t *testing.T) {
	cs, disc := podFixture()
	nginx := appPod("nginx-1", "registry.k8s.io/ingress-nginx/controller:v1.9.4")
	srv := servePods(cs, nginx)
	_ = srv
	k := loadKB(t)
	pass, clk := newPassCache(3, time.Hour)
	clients := Clients{Kube: cs, Discovery: disc}
	run := func() string {
		inv := Collect(context.Background(), clients, k, Options{PodPass: pass})
		clk.t = clk.t.Add(10 * time.Minute)
		return addOnVersions(inv)["ingress-nginx"]
	}

	if v := run(); v != "1.9.4" {
		t.Fatalf("tick 1 (a pass): version %q, want 1.9.4", v)
	}
	nginx.Spec.Containers[0].Image = "registry.k8s.io/ingress-nginx/controller:v1.10.0" // upgraded
	for tick := 2; tick <= 3; tick++ {
		if v := run(); v != "1.9.4" {
			t.Errorf("tick %d: version %q, want the old 1.9.4 from the reused pass (at most N-1 = 2 ticks)", tick, v)
		}
	}
	if v := run(); v != "1.10.0" {
		t.Errorf("tick 4 (a pass): version %q, want 1.10.0", v)
	}
}

// The registry is applied to the reused evidence every collection, so a
// knowledge-base update does not wait for a full pass.
func TestPodPassAppliesTheCurrentRegistryToReusedEvidence(t *testing.T) {
	cs, disc := podFixture()
	servePods(cs, append(tickPods(), appPod("mine-1", "example.com/team/mine:v2.3.4"))...)
	k := loadKB(t)
	pass, _ := newPassCache(3, time.Hour)
	clients := Clients{Kube: cs, Discovery: disc}

	first := Collect(context.Background(), clients, k, Options{PodPass: pass})
	if _, ok := addOnVersions(first)["mine"]; ok {
		t.Fatalf("add-ons = %v, want no mine yet", addOnVersions(first))
	}
	if !slices.Contains(first.UnrecognizedImages, "example.com/team/mine") {
		t.Fatalf("unrecognized = %v, want example.com/team/mine", first.UnrecognizedImages)
	}

	k2 := k
	k2.AddOns = append(slices.Clone(k.AddOns), registry.AddOn{ID: "mine", Matchers: registry.Matchers{Images: []string{"team/mine"}}})
	second := Collect(context.Background(), clients, k2, Options{PodPass: pass})
	if second.AddOnEvidenceAgeSeconds == 0 {
		t.Fatal("the second collection read every pod, want it to reuse the pass")
	}
	if v := addOnVersions(second)["mine"]; v != "2.3.4" {
		t.Errorf("add-ons = %v, want mine 2.3.4 from the reused evidence", addOnVersions(second))
	}
	if slices.Contains(second.UnrecognizedImages, "example.com/team/mine") {
		t.Errorf("unrecognized = %v, want mine claimed", second.UnrecognizedImages)
	}
}

// A pass older than the maximum age is not reused, whatever the count says.
func TestPodPassMaxAgeForcesAFullPass(t *testing.T) {
	cs, disc := podFixture()
	srv := servePods(cs, tickPods()...)
	k := loadKB(t)
	pass, clk := newPassCache(100, 25*time.Minute)
	clients := Clients{Kube: cs, Discovery: disc}

	var lists []int
	for range 5 {
		srv.calls = nil
		Collect(context.Background(), clients, k, Options{PodPass: pass})
		lists = append(lists, srv.allNamespaceLists())
		clk.t = clk.t.Add(10 * time.Minute)
	}
	// ages at each tick: pass, 10m, 20m, 30m (over 25m: a pass), 10m.
	if want := []int{1, 0, 0, 1, 0}; !slices.Equal(lists, want) {
		t.Errorf("lists of the other namespaces per tick = %v, want %v", lists, want)
	}
}

// A pass that failed is not kept: the next collection lists every pod, and
// a pass that read only some pages is the same. The collection after a
// complete pass reuses it again.
func TestPodPassFailedOrPartialPassForcesTheNextOne(t *testing.T) {
	for _, tc := range []struct {
		name    string
		perPage int
		fail    func(ns string, o metav1.ListOptions) error
	}{
		{"first page", 0, func(ns string, _ metav1.ListOptions) error {
			if ns == "" {
				return errors.New("etcdserver: request timed out")
			}
			return nil
		}},
		{"later page", 1, func(ns string, o metav1.ListOptions) error {
			if ns == "" && o.Continue != "" {
				return errors.New("etcdserver: request timed out")
			}
			return nil
		}},
	} {
		for _, held := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "", true: ", with an earlier pass held"}[held], func(t *testing.T) {
				cs, disc := podFixture()
				srv := servePods(cs, append(tickPods(), appPod("nginx-2", "example.com/app:v1"))...)
				srv.perPage = tc.perPage
				failing := !held
				srv.failList = func(_ int, ns string, o metav1.ListOptions) error {
					if failing {
						return tc.fail(ns, o)
					}
					return nil
				}
				k := loadKB(t)
				pass, _ := newPassCache(5, time.Hour)
				clients := Clients{Kube: cs, Discovery: disc}

				if held {
					// A pass that would be reused, if the failed one did not drop it:
					// the kube-system list fails too, so the next collection lists
					// every pod, as the cache forces, and that is the failing pass.
					Collect(context.Background(), clients, k, Options{PodPass: pass})
					if !pass.Held() {
						t.Fatal("no earlier pass held")
					}
					failing = true
					failKubeSystem := srv.failList
					srv.failList = func(call int, ns string, o metav1.ListOptions) error {
						if failing && ns == "kube-system" {
							return errors.New("forbidden")
						}
						return failKubeSystem(call, ns, o)
					}
				}
				inv := Collect(context.Background(), clients, k, Options{PodPass: pass})
				if st := inv.Capabilities[inventory.CapAddOns]; !st.Partial {
					t.Fatalf("addons capability = %+v, want partial: the pass failed", st)
				}
				if pass.Held() {
					t.Fatal("the cache holds a failed pass")
				}

				failing = false
				srv.calls = nil
				inv = Collect(context.Background(), clients, k, Options{PodPass: pass})
				if srv.allNamespaceLists() != 1 || inv.AddOnEvidenceAgeSeconds != 0 {
					t.Errorf("after a failed pass: %d lists, age %d; want a full pass", srv.allNamespaceLists(), inv.AddOnEvidenceAgeSeconds)
				}
				if st := inv.Capabilities[inventory.CapAddOns]; st.Partial {
					t.Fatalf("addons capability = %+v, want complete", st)
				}

				srv.calls = nil
				inv = Collect(context.Background(), clients, k, Options{PodPass: pass})
				if srv.allNamespaceLists() != 0 || inv.AddOnEvidenceAgeSeconds == 0 {
					t.Errorf("after a complete pass: %d lists, age %d; want it reused", srv.allNamespaceLists(), inv.AddOnEvidenceAgeSeconds)
				}
			})
		}
	}
}

// A pass that fails after an earlier one succeeded drops the earlier
// evidence too: the failure is reported as it always was, with no pod
// evidence, and nothing is reused later from before it.
func TestPodPassFailureDropsTheEarlierPass(t *testing.T) {
	cs, disc := podFixture()
	srv := servePods(cs, tickPods()...)
	k := loadKB(t)
	pass, clk := newPassCache(2, time.Hour)
	clients := Clients{Kube: cs, Discovery: disc}

	Collect(context.Background(), clients, k, Options{PodPass: pass}) // pass
	Collect(context.Background(), clients, k, Options{PodPass: pass}) // reuse
	srv.failList = func(_ int, ns string, _ metav1.ListOptions) error {
		if ns == "" {
			return errors.New("boom")
		}
		return nil
	}
	clk.t = clk.t.Add(10 * time.Minute)
	inv := Collect(context.Background(), clients, k, Options{PodPass: pass}) // pass (count reached), fails
	if st := inv.Capabilities[inventory.CapAddOns]; !st.Partial {
		t.Fatalf("addons capability = %+v, want partial", st)
	}
	if pass.Held() {
		t.Error("the cache still holds the earlier pass after a failed one")
	}
}

// A collection that lists every pod drops the held pass before its first
// list, not when the new pass is recorded: the old pass is never held beside
// the new evidence, and a pass that fails at any page, or is cancelled,
// leaves nothing held.
func TestPodPassOldPassIsDroppedBeforeTheNewOneIsRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failAt  string // "": the pass succeeds; "first" or "later" page
		wantNew bool
	}{
		{"complete", "", true},
		{"fails on the first page", "first", false},
		{"fails on a later page", "later", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, disc := podFixture()
			srv := servePods(cs, append(tickPods(), appPod("nginx-2", "example.com/app:v1"), appPod("nginx-3", "example.com/app:v2"))...)
			srv.perPage = 1
			k := loadKB(t)
			pass, clk := newPassCache(2, time.Hour)
			clients := Clients{Kube: cs, Discovery: disc}

			Collect(context.Background(), clients, k, Options{PodPass: pass}) // pass
			Collect(context.Background(), clients, k, Options{PodPass: pass}) // reuse
			if !pass.Held() {
				t.Fatal("no pass held")
			}
			clk.t = clk.t.Add(time.Minute)

			// Each list of the other namespaces says whether a pass was held
			// when it was made.
			var heldDuring []bool
			srv.failList = func(_ int, ns string, o metav1.ListOptions) error {
				if ns != "" {
					return nil
				}
				heldDuring = append(heldDuring, pass.Held())
				if (tc.failAt == "first" && o.Continue == "") || (tc.failAt == "later" && o.Continue != "") {
					return errors.New("etcdserver: request timed out")
				}
				return nil
			}
			Collect(context.Background(), clients, k, Options{PodPass: pass}) // pass (count reached)

			if len(heldDuring) == 0 {
				t.Fatal("the second collection did not list the pods")
			}
			for i, h := range heldDuring {
				if h {
					t.Errorf("list %d of the pass was made with the earlier pass still held", i+1)
				}
			}
			if pass.Held() != tc.wantNew {
				t.Errorf("Held() after the pass = %v, want %v", pass.Held(), tc.wantNew)
			}
		})
	}
}

// Without versions' list of the kube-system pods the add-ons list every
// pod, kube-system included, and the held pass does not hold those pods:
// that collection lists everything and reuses nothing.
func TestPodPassNeedsTheKubeSystemListOfThisCollection(t *testing.T) {
	cs, disc := podFixture()
	srv := servePods(cs, tickPods()...)
	k := loadKB(t)
	pass, _ := newPassCache(5, time.Hour)
	clients := Clients{Kube: cs, Discovery: disc}

	Collect(context.Background(), clients, k, Options{PodPass: pass})
	srv.failList = func(_ int, ns string, _ metav1.ListOptions) error {
		if ns == "kube-system" {
			return errors.New("forbidden")
		}
		return nil
	}
	srv.calls = nil
	inv := Collect(context.Background(), clients, k, Options{PodPass: pass})
	if inv.AddOnEvidenceAgeSeconds != 0 {
		t.Errorf("age = %d, want a full read when versions could not list kube-system", inv.AddOnEvidenceAgeSeconds)
	}
	if got := addOnIDs(inv); !reflect.DeepEqual(got, []string{"cilium", "ingress-nginx"}) {
		t.Errorf("add-ons = %v, want cilium (a kube-system pod) and ingress-nginx", got)
	}
	if last := srv.calls[len(srv.calls)-1]; last.namespace != "" || last.fieldSelector != "" {
		t.Errorf("last pod list = %+v, want the cluster-wide list with kube-system", last)
	}
	// The full read was a complete pass, and is held for the collections after it.
	srv.failList, srv.calls = nil, nil
	inv = Collect(context.Background(), clients, k, Options{PodPass: pass})
	if inv.AddOnEvidenceAgeSeconds == 0 || srv.allNamespaceLists() != 0 {
		t.Errorf("age %d, %d lists: want the pass reused once kube-system lists again", inv.AddOnEvidenceAgeSeconds, srv.allNamespaceLists())
	}
}

// Every of 1 (and a nil cache) reads every pod every collection, as before.
func TestPodPassEveryOneReadsEveryPod(t *testing.T) {
	for name, pass := range map[string]*PodPassCache{"every 1": NewPodPassCache(1, time.Hour), "nil": nil, "zero": NewPodPassCache(0, time.Hour)} {
		cs, disc := podFixture()
		srv := servePods(cs, tickPods()...)
		for range 3 {
			inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{PodPass: pass})
			if inv.AddOnEvidenceAgeSeconds != 0 {
				t.Errorf("%s: age = %d, want none", name, inv.AddOnEvidenceAgeSeconds)
			}
		}
		if got := srv.allNamespaceLists(); got != 3 {
			t.Errorf("%s: %d lists of the other namespaces in 3 collections, want 3", name, got)
		}
		if pass.Held() {
			t.Errorf("%s: holds a pass", name)
		}
	}
}

// The cache keeps each distinct pair and labelled pod once, and none of
// kube-system's, and detection from that is the detection from the whole.
func TestPodPassRecordKeepsDistinctEvidenceOutsideKubeSystem(t *testing.T) {
	labels := appLabels{name: "ingress-nginx", version: "1.9.4"}
	ev := addOnEvidence{}
	for range 50 {
		ev.addPod("ingress-nginx", map[string]string{"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/version": "1.9.4"},
			[]string{"registry.k8s.io/ingress-nginx/controller:v1.9.4"})
		ev.addPod("kube-system", nil, []string{"quay.io/cilium/cilium:v1.14.0"})
	}
	ev.addPod("ingress-nginx", map[string]string{"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/version": "1.9.5"},
		[]string{"registry.k8s.io/ingress-nginx/controller:v1.9.4"})
	c, _ := newPassCache(3, time.Hour)
	c.record(ev, c.now(), "")

	images, labelled, _, ok := c.reuse("")
	if !ok {
		t.Fatal("a recorded pass was not reused")
	}
	if want := []nsImage{{"ingress-nginx", "registry.k8s.io/ingress-nginx/controller:v1.9.4"}}; !reflect.DeepEqual(images, want) {
		t.Errorf("images = %+v, want %+v", images, want)
	}
	if len(labelled) != 2 || labelled[0].Labels != labels || labelled[1].Labels.version != "1.9.5" {
		t.Errorf("labelled = %+v, want the two distinct label sets in first-seen order", labelled)
	}

	whole := ev
	whole.images, whole.labelled = slices.Concat(ev.images), slices.Concat(ev.labelled)
	kept := addOnEvidence{images: slices.Concat(images, []nsImage{{"kube-system", "quay.io/cilium/cilium:v1.14.0"}}),
		labelled: labelled}
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	a, au := matchAddOns(whole, k.AddOns)
	b, bu := matchAddOns(kept, k.AddOns)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(au, bu) {
		t.Errorf("matchAddOns over the kept evidence = %+v / %v, want that of the whole %+v / %v", b, bu, a, au)
	}
}

// The Helm releases and GitOps resources are read in every collection, and
// the add-ons join them with the pods of the reused pass. A release that
// changed since the pass would pair its new appVersion with the old pods,
// and report the old version as a second install on another release line,
// so a change in the releases that name an add-on forces a full pass.
func TestPodPassHelmUpgradeForcesAFullPass(t *testing.T) {
	cs, _ := podFixture()
	pod := appPod("nginx-1", "registry.k8s.io/ingress-nginx/controller:v1.9.4")
	srv := servePods(cs, pod)
	addons := loadKB(t).AddOns
	pass, clk := newPassCache(3, time.Hour)
	release := func(chart, app string, rev int) inventory.HelmRelease {
		return inventory.HelmRelease{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx",
			ChartVersion: chart, AppVersion: app, Status: "deployed", Revision: rev}
	}
	tick := func(releases []inventory.HelmRelease, gitops ...inventory.GitOpsChart) (inventory.Inventory, int) {
		srv.calls = nil
		inv := inventory.Inventory{HelmReleases: releases, GitOpsCharts: gitops, Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
		if err := collectAddOnsFrom(context.Background(), cs, addons, &inv, &kubeSystemPods{read: true}, pass); err != nil {
			t.Fatal(err)
		}
		clk.t = clk.t.Add(10 * time.Minute)
		return inv, srv.allNamespaceLists()
	}
	ingress := func(inv inventory.Inventory) []inventory.AddOnInstance {
		var out []inventory.AddOnInstance
		for _, a := range inv.AddOns {
			if a.ID == "ingress-nginx" {
				out = append(out, a)
			}
		}
		return out
	}

	if inv, lists := tick([]inventory.HelmRelease{release("4.8.3", "1.9.4", 1)}); lists != 1 || len(ingress(inv)) != 1 {
		t.Fatalf("tick 1: %d lists, %+v; want a pass and one install", lists, ingress(inv))
	}
	// Unchanged: the pass is reused.
	if _, lists := tick([]inventory.HelmRelease{release("4.8.3", "1.9.4", 1)}); lists != 0 {
		t.Fatalf("tick 2: %d lists, want the pass reused while the releases are unchanged", lists)
	}
	// The release and the pods are upgraded across a release line.
	pod.Spec.Containers[0].Image = "registry.k8s.io/ingress-nginx/controller:v1.10.0"
	inv, lists := tick([]inventory.HelmRelease{release("4.10.0", "1.10.0", 2)})
	if lists != 1 {
		t.Fatalf("tick 3: %d lists, want a full pass: the release changed", lists)
	}
	got := ingress(inv)
	if len(got) != 1 || got[0].Version != "1.10.0" || got[0].ChartVersion != "4.10.0" || got[0].Source != "chart" {
		t.Errorf("after the upgrade: %+v, want one install at 1.10.0, chart 4.10.0 (no phantom 1.9.4 from the old pods)", got)
	}
	if inv.AddOnEvidenceAgeSeconds != 0 {
		t.Errorf("age = %d, want none after a full pass", inv.AddOnEvidenceAgeSeconds)
	}
	// And the new pass is reused from then on.
	if _, lists := tick([]inventory.HelmRelease{release("4.10.0", "1.10.0", 2)}); lists != 0 {
		t.Errorf("tick 4: %d lists, want the new pass reused", lists)
	}
	// A release with no appVersion takes its version from the pods, so a
	// revision it was rolled to forces a pass too.
	if _, lists := tick([]inventory.HelmRelease{release("4.10.0", "1.10.0", 3)}); lists != 1 {
		t.Errorf("a new revision: %d lists, want a full pass", lists)
	}
}

// A Helm release of no add-on, however often it is upgraded, does not cost a
// pod pass: only the releases and charts the registry names count.
func TestPodPassIgnoresReleasesOfNoAddOn(t *testing.T) {
	cs, _ := podFixture()
	srv := servePods(cs, appPod("nginx-1", "registry.k8s.io/ingress-nginx/controller:v1.9.4"))
	addons := loadKB(t).AddOns
	pass, clk := newPassCache(10, time.Hour)
	for rev := 1; rev <= 4; rev++ {
		srv.calls = nil
		inv := inventory.Inventory{
			HelmReleases: []inventory.HelmRelease{{Name: "shop", Namespace: "shop", ChartName: "shop", ChartVersion: "1." + strconv.Itoa(rev) + ".0", AppVersion: "1." + strconv.Itoa(rev) + ".0", Status: "deployed", Revision: rev}},
			GitOpsCharts: []inventory.GitOpsChart{{Tool: inventory.GitOpsArgoCD, Name: "shop", Chart: "shop", Version: "1." + strconv.Itoa(rev) + ".0"}},
			Capabilities: map[inventory.Capability]inventory.CapabilityStatus{},
		}
		if err := collectAddOnsFrom(context.Background(), cs, addons, &inv, &kubeSystemPods{read: true}, pass); err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{true: 1, false: 0}[rev == 1]; srv.allNamespaceLists() != want {
			t.Errorf("revision %d: %d lists, want %d", rev, srv.allNamespaceLists(), want)
		}
		clk.t = clk.t.Add(10 * time.Minute)
	}
}

// A GitOps chart reference that changed, or appeared, takes the next
// collection to a full pass: its version is evidence beside the pods'.
func TestPodPassGitOpsChartChangeForcesAFullPass(t *testing.T) {
	cs, _ := podFixture()
	srv := servePods(cs, appPod("nginx-1", "registry.k8s.io/ingress-nginx/controller:v1.9.4"))
	addons := loadKB(t).AddOns
	pass, clk := newPassCache(10, time.Hour)
	app := func(version string) inventory.GitOpsChart {
		return inventory.GitOpsChart{Tool: inventory.GitOpsArgoCD, Name: "ingress", Namespace: "argocd", Target: "ingress-nginx", Chart: "ingress-nginx", Version: version}
	}
	for i, tc := range []struct {
		charts []inventory.GitOpsChart
		lists  int
	}{
		{nil, 1},
		{nil, 0},
		{[]inventory.GitOpsChart{app("4.8.3")}, 1}, // appeared
		{[]inventory.GitOpsChart{app("4.8.3")}, 0},
		{[]inventory.GitOpsChart{app("4.10.0")}, 1}, // bumped
		{nil, 1}, // gone
		{nil, 0},
	} {
		srv.calls = nil
		inv := inventory.Inventory{GitOpsCharts: tc.charts, Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
		if err := collectAddOnsFrom(context.Background(), cs, addons, &inv, &kubeSystemPods{read: true}, pass); err != nil {
			t.Fatal(err)
		}
		if srv.allNamespaceLists() != tc.lists {
			t.Errorf("step %d: %d lists, want %d", i, srv.allNamespaceLists(), tc.lists)
		}
		clk.t = clk.t.Add(10 * time.Minute)
	}
}
