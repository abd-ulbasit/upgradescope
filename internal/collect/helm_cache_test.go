package collect

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// The agent asks the apiserver again on every tick, and the Helm collector's
// one GET per release was the cost that grew with releases: 1,000 releases
// took 50 to 80 s of a tick on an apiserver on the same host (#71,
// docs/operations/scale.md), enough to reach the step deadline. A Helm
// storage object's payload does not change once written, so a long-running
// caller (the agent) keeps what it decoded, keyed by the object's UID and
// resourceVersion, and a tick fetches only objects it has not decoded.

// helmGets returns the names of the typed client's GETs so far.
func helmGets(kube interface{ Actions() []clienttesting.Action }) []string {
	var got []string
	for _, a := range kube.Actions() {
		if g, ok := a.(clienttesting.GetAction); ok {
			got = append(got, g.GetResource().Resource+"/"+g.GetName())
		}
	}
	return got
}

func cacheRevs(t *testing.T) []runtime.Object {
	t.Helper()
	return []runtime.Object{
		helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 2, status: "deployed", chart: "ingress-nginx", chartVersion: "4.7.1", appVersion: "1.8.4", uid: "u1", rv: "100"}),
		helmSecret(t, helmRev{ns: "cert-manager", release: "cert-manager", rev: 1, status: "deployed", chart: "cert-manager", chartVersion: "v1.13.0", appVersion: "v1.13.0", uid: "u2", rv: "101"}),
	}
}

func collectHelmCached(t *testing.T, cache *HelmCache, lifecycle []kb.APILifecycleEntry, objs ...runtime.Object) (inventory.Inventory, []string, error) {
	t.Helper()
	kube, meta := helmClients(t, objs...)
	var inv inventory.Inventory
	err := collectHelmWith(context.Background(), kube, meta, lifecycle, cache, &inv)
	return inv, helmGets(kube), err
}

func TestHelmCacheSkipsReleasesItAlreadyDecoded(t *testing.T) {
	cache := NewHelmCache()
	first, gets, err := collectHelmCached(t, cache, nil, cacheRevs(t)...)
	if err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	if len(gets) != 2 {
		t.Fatalf("a cold cache: %v, want a GET per release", gets)
	}
	second, gets, err := collectHelmCached(t, cache, nil, cacheRevs(t)...)
	if err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	if len(gets) != 0 {
		t.Errorf("a warm cache and nothing changed: GETs %v, want none", gets)
	}
	if !reflect.DeepEqual(first.HelmReleases, second.HelmReleases) || len(second.HelmReleases) != 2 {
		t.Errorf("warm = %#v\ncold = %#v", second.HelmReleases, first.HelmReleases)
	}
}

func TestHelmCacheRefetchesWhatChanged(t *testing.T) {
	cache := NewHelmCache()
	collectHelmCached(t, cache, nil, cacheRevs(t)...)

	// ingress-nginx is upgraded (a new revision object) and cert-manager's
	// object is replaced (a new UID and resourceVersion, same name).
	objs := []runtime.Object{
		helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 3, status: "deployed", chart: "ingress-nginx", chartVersion: "4.8.0", appVersion: "1.9.0", uid: "u3", rv: "200"}),
		helmSecret(t, helmRev{ns: "cert-manager", release: "cert-manager", rev: 1, status: "deployed", chart: "cert-manager", chartVersion: "v1.14.0", appVersion: "v1.14.0", uid: "u4", rv: "201"}),
	}
	inv, gets, err := collectHelmCached(t, cache, nil, objs...)
	if err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	if want := "secrets/sh.helm.release.v1.cert-manager.v1 secrets/sh.helm.release.v1.ingress-nginx.v3"; strings.Join(slices.Sorted(slices.Values(gets)), " ") != want {
		t.Errorf("GETs %v, want exactly the two changed objects", gets)
	}
	versions := map[string]string{}
	for _, r := range inv.HelmReleases {
		versions[r.Name] = r.ChartVersion
	}
	if versions["ingress-nginx"] != "4.8.0" || versions["cert-manager"] != "v1.14.0" {
		t.Errorf("chart versions %v: the new content must win over the cached one", versions)
	}

	// Same UID, a new resourceVersion: the object was written again.
	bump := []runtime.Object{objs[0], helmSecret(t, helmRev{ns: "cert-manager", release: "cert-manager", rev: 1, status: "deployed", chart: "cert-manager", chartVersion: "v1.15.0", uid: "u4", rv: "300"})}
	inv, gets, _ = collectHelmCached(t, cache, nil, bump...)
	if len(gets) != 1 || !strings.HasSuffix(gets[0], "cert-manager.v1") {
		t.Errorf("GETs %v, want only the rewritten cert-manager object", gets)
	}
	for _, r := range inv.HelmReleases {
		if r.Name == "cert-manager" && r.ChartVersion != "v1.15.0" {
			t.Errorf("cert-manager chart %s, want v1.15.0", r.ChartVersion)
		}
	}
}

func TestHelmCacheKeepsOnlyWhatIsStillInstalled(t *testing.T) {
	cache := NewHelmCache()
	collectHelmCached(t, cache, nil, cacheRevs(t)...)
	if cache.Len() != 2 {
		t.Fatalf("cache holds %d, want 2", cache.Len())
	}
	// cert-manager is uninstalled; ingress-nginx's installed revision moves on.
	collectHelmCached(t, cache, nil,
		helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 3, status: "deployed", chart: "ingress-nginx", chartVersion: "4.8.0", uid: "u3", rv: "200"}),
	)
	if cache.Len() != 1 {
		t.Errorf("cache holds %d after a release left and one moved on, want 1: memory must follow the cluster, not grow with its history", cache.Len())
	}
}

// Nothing is remembered about a payload that could not be fetched: the next
// tick tries again.
func TestHelmCacheDoesNotRememberAFailedFetch(t *testing.T) {
	cache := NewHelmCache()
	objs := cacheRevs(t)
	kube, meta := helmClients(t, objs...)
	fail := true
	kube.PrependReactor("get", "secrets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if fail && a.(clienttesting.GetAction).GetName() == "sh.helm.release.v1.cert-manager.v1" {
			return true, nil, errors.New("etcdserver: request timed out")
		}
		return false, nil, nil
	})
	var inv inventory.Inventory
	err := collectHelmWith(context.Background(), kube, meta, nil, cache, &inv)
	var pe partialError
	if !errors.As(err, &pe) || !pe.incomplete || len(inv.HelmReleases) != 1 {
		t.Fatalf("err = %v, %d releases: want a partial result with one release", err, len(inv.HelmReleases))
	}
	fail = false
	kube.ClearActions()
	inv = inventory.Inventory{}
	if err := collectHelmWith(context.Background(), kube, meta, nil, cache, &inv); err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	if got := helmGets(kube); len(got) != 1 || !strings.HasSuffix(got[0], "cert-manager.v1") {
		t.Errorf("GETs %v, want only the release that failed before", got)
	}
	if len(inv.HelmReleases) != 2 {
		t.Errorf("%d releases, want both", len(inv.HelmReleases))
	}
}

// A release that cannot be decoded, or whose manifest is not fully parsed,
// stays a gap on every tick, with the same words, and is not downloaded
// again to say so.
func TestHelmCacheKeepsReportingWhatItCouldNotRead(t *testing.T) {
	corrupt := helmSecret(t, helmRev{ns: "shop", release: "broken", rev: 1, status: "deployed", uid: "u9", rv: "9"})
	corrupt.Data["release"] = []byte("%%% not base64 %%%")
	good := helmSecret(t, helmRev{ns: "cert-manager", release: "cert-manager", rev: 1, status: "deployed", chart: "cert-manager", chartVersion: "v1.13.0", uid: "u2", rv: "101"})

	cache := NewHelmCache()
	_, _, coldErr := collectHelmCached(t, cache, nil, good, corrupt)
	_, gets, warmErr := collectHelmCached(t, cache, nil, good, corrupt)
	if len(gets) != 0 {
		t.Errorf("GETs %v, want none: the corrupt payload is not downloaded again", gets)
	}
	var cold, warm partialError
	if !errors.As(coldErr, &cold) || !errors.As(warmErr, &warm) {
		t.Fatalf("errors %v, %v: want partial results", coldErr, warmErr)
	}
	if !reflect.DeepEqual(cold, warm) || !strings.Contains(warm.msg, "1 release(s) not decodable, first shop/broken: ") {
		t.Errorf("warm = %+v\ncold = %+v: the gap must read the same", warm, cold)
	}
}

// What the knowledge base flags decides which manifest objects a cached
// entry records, so a different set is a different cache.
func TestHelmCacheIsInvalidatedByADifferentKnowledgeBase(t *testing.T) {
	manifest := "apiVersion: flowcontrol.apiserver.k8s.io/v1beta3\nkind: FlowSchema\nmetadata:\n  name: batch\n"
	obj := helmSecret(t, helmRev{ns: "platform", release: "apf", rev: 1, status: "deployed", chart: "apf", manifest: manifest, uid: "u1", rv: "1"})
	v := func(m int) *inventory.Version { return &inventory.Version{Major: 1, Minor: m} }
	flagged := []kb.APILifecycleEntry{{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Introduced: *v(26), Deprecated: v(29), Removed: v(32)}}

	cache := NewHelmCache()
	inv, _, _ := collectHelmCached(t, cache, nil, obj)
	if n := len(inv.HelmReleases[0].ManifestAPIs); n != 0 {
		t.Fatalf("nothing flagged: %d manifest API rows, want none", n)
	}
	inv, gets, _ := collectHelmCached(t, cache, flagged, obj)
	if len(gets) != 1 || len(inv.HelmReleases[0].ManifestAPIs) != 1 {
		t.Errorf("GETs %v, %d rows: a new knowledge base must re-read the release and find the FlowSchema", gets, len(inv.HelmReleases[0].ManifestAPIs))
	}
	inv, gets, _ = collectHelmCached(t, cache, flagged, obj)
	if len(gets) != 0 || len(inv.HelmReleases[0].ManifestAPIs) != 1 {
		t.Errorf("GETs %v, %d rows: the same knowledge base is served from the cache", gets, len(inv.HelmReleases[0].ManifestAPIs))
	}
}

// An inventory handed to the caller can be changed (cut, sorted) without
// reaching into the cache.
func TestHelmCacheHandsOutCopies(t *testing.T) {
	manifest := "apiVersion: flowcontrol.apiserver.k8s.io/v1beta3\nkind: FlowSchema\nmetadata:\n  name: batch\n"
	obj := helmSecret(t, helmRev{ns: "platform", release: "apf", rev: 1, status: "deployed", chart: "apf", manifest: manifest, uid: "u1", rv: "1"})
	v := func(m int) *inventory.Version { return &inventory.Version{Major: 1, Minor: m} }
	flagged := []kb.APILifecycleEntry{{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Introduced: *v(26), Deprecated: v(29), Removed: v(32)}}
	cache := NewHelmCache()
	first, _, _ := collectHelmCached(t, cache, flagged, obj)
	first.HelmReleases[0].ManifestAPIs[0].Objects[0].Name = "mutated"
	first.HelmReleases[0].ManifestAPIs[0].Namespaces["x"] = 7
	second, _, _ := collectHelmCached(t, cache, flagged, obj)
	if got := second.HelmReleases[0].ManifestAPIs[0]; got.Objects[0].Name != "batch" || len(got.Namespaces) != 1 {
		t.Errorf("the cache served %+v after the caller changed its copy", got)
	}
}

// Without a cache (a one-shot scan) every release is read, as before; and
// a payload with no UID or resourceVersion to key it by is not remembered.
func TestHelmCacheNeedsAnIdentityToKeyBy(t *testing.T) {
	cache := NewHelmCache()
	noID := []runtime.Object{helmSecret(t, helmRev{ns: "a", release: "a", rev: 1, status: "deployed", chart: "a"})}
	collectHelmCached(t, cache, nil, noID...)
	_, gets, _ := collectHelmCached(t, cache, nil, noID...)
	if len(gets) != 1 || cache.Len() != 0 {
		t.Errorf("GETs %v, cache %d: an object with no resourceVersion is never served from the cache", gets, cache.Len())
	}
	_, gets, _ = collectHelmCached(t, nil, nil, cacheRevs(t)...)
	_, gets2, _ := collectHelmCached(t, nil, nil, cacheRevs(t)...)
	if len(gets) != 2 || len(gets2) != 2 {
		t.Errorf("a nil cache: GETs %v, %v, want a GET per release each time", gets, gets2)
	}
}

// Both drivers are cached, and the ConfigMap driver's objects are keyed
// apart from Secrets of the same name.
func TestHelmCacheCoversTheConfigMapDriver(t *testing.T) {
	cm := helmConfigMap(t, helmRev{ns: "apps", release: "web", rev: 1, status: "deployed", chart: "web", chartVersion: "1.0.0", uid: "c1", rv: "5"})
	cache := NewHelmCache()
	collectHelmCached(t, cache, nil, cm)
	inv, gets, _ := collectHelmCached(t, cache, nil, cm)
	if len(gets) != 0 || len(inv.HelmReleases) != 1 || inv.HelmReleases[0].ChartName != "web" {
		t.Errorf("GETs %v, releases %+v: want the ConfigMap release served from the cache", gets, inv.HelmReleases)
	}
	if got := fmt.Sprint(cache.Len()); got != "1" {
		t.Errorf("cache holds %s", got)
	}
}

// A driver whose list failed this tick says nothing about its objects, so
// the entries it holds survive to the tick that lists it again: nothing is
// refetched because of a flaky list.
func TestHelmCacheKeepsADriversEntriesWhenItsListFails(t *testing.T) {
	cm := helmConfigMap(t, helmRev{ns: "apps", release: "web", rev: 1, status: "deployed", chart: "web", chartVersion: "1.0.0", uid: "c1", rv: "5"})
	sec := helmSecret(t, helmRev{ns: "cert-manager", release: "cert-manager", rev: 1, status: "deployed", chart: "cert-manager", chartVersion: "v1.13.0", uid: "u2", rv: "101"})
	cache := NewHelmCache()
	collectHelmCached(t, cache, nil, cm, sec)
	if cache.Len() != 2 {
		t.Fatalf("cache holds %d after the first tick, want 2", cache.Len())
	}

	// The ConfigMap list fails: its release is not seen, and must not be
	// forgotten either.
	kube, meta := helmClients(t, cm, sec)
	failList := true
	meta.PrependReactor("list", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failList {
			return true, nil, errors.New("etcdserver: request timed out")
		}
		return false, nil, nil
	})
	var inv inventory.Inventory
	err := collectHelmWith(context.Background(), kube, meta, nil, cache, &inv)
	if !errors.As(err, new(partialError)) || len(inv.HelmReleases) != 1 {
		t.Fatalf("err = %v, %d releases: want a partial result with the Secret release", err, len(inv.HelmReleases))
	}
	if cache.Len() != 2 {
		t.Fatalf("cache holds %d after the ConfigMap list failed, want the ConfigMap entry kept (2)", cache.Len())
	}

	// The list works again: the release comes back without a GET.
	failList = false
	kube.ClearActions()
	inv = inventory.Inventory{}
	if err := collectHelmWith(context.Background(), kube, meta, nil, cache, &inv); err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	if got := helmGets(kube); len(got) != 0 {
		t.Errorf("GETs %v, want none: the entry survived the failed list", got)
	}
	if len(inv.HelmReleases) != 2 {
		t.Errorf("%d releases, want both", len(inv.HelmReleases))
	}
}
