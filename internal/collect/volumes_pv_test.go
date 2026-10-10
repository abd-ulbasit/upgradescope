package collect

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #362: live, the volumes capability also lists PersistentVolumes (each
// one's in-tree source, counted under its claim's namespace when bound)
// and StorageClasses (an in-tree provisioner, counted for its plugin).

func testPV(name, claimNS string, src corev1.PersistentVolumeSource) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:   corev1.PersistentVolumeSpec{PersistentVolumeSource: src},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable}}
	if claimNS != "" {
		pv.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: claimNS, Name: "claim"}
		pv.Status.Phase = corev1.VolumeBound
	}
	return pv
}

func testSC(name, provisioner string) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: provisioner}
}

func rbdSource() corev1.PersistentVolumeSource {
	return corev1.PersistentVolumeSource{RBD: &corev1.RBDPersistentVolumeSource{CephMonitors: []string{"10.0.0.1:6789"}, RBDImage: "x", RBDPool: "rbd"}}
}

// volumeObjects are the issue's PersistentVolumes and StorageClasses: an
// rbd PV bound to a claim in data, an awsElasticBlockStore PV bound in
// shop, an unbound rbd PV, a PV once bound whose claim is gone (Released,
// its claimRef kept), PVs of a CSI driver and a hostPath (not in-tree
// plugins the dataset lists), an rbd StorageClass (ignored by annotation)
// and a CSI one.
func volumeObjects() []runtime.Object {
	released := testPV("pv-released", "gone", corev1.PersistentVolumeSource{AWSElasticBlockStore: &corev1.AWSElasticBlockStoreVolumeSource{VolumeID: "vol-2"}})
	released.Status.Phase = corev1.VolumeReleased
	sc := testSC("sc-rbd", "kubernetes.io/rbd")
	sc.Annotations = map[string]string{IgnoreAnnotation: "until=2099-01-01"}
	return []runtime.Object{
		testPV("pv-rbd", "data", rbdSource()),
		testPV("pv-ebs", "shop", corev1.PersistentVolumeSource{AWSElasticBlockStore: &corev1.AWSElasticBlockStoreVolumeSource{VolumeID: "vol-1"}}),
		testPV("pv-unbound", "", rbdSource()),
		released,
		testPV("pv-csi", "shop", corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-3"}}),
		testPV("pv-host", "", corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/x"}}),
		sc,
		testSC("sc-csi", "ebs.csi.aws.com"),
	}
}

// wantObjectVolumes is volumeTickPods' rows with volumeObjects' added.
func wantObjectVolumes() []inventory.VolumePluginUse {
	return []inventory.VolumePluginUse{
		{Plugin: "awsElasticBlockStore", Count: 3, Namespaces: map[string]int{"shop": 2, "": 1},
			Objects: []inventory.ObjectRef{{Name: "pv-ebs"}, {Name: "pv-released"}}},
		{Plugin: "gitRepo", Count: 1, Namespaces: map[string]int{"kube-system": 1}},
		{Plugin: "glusterfs", Count: 2, Namespaces: map[string]int{"shop": 2}},
		{Plugin: "rbd", Count: 3, Namespaces: map[string]int{"data": 1, "": 2},
			Objects: []inventory.ObjectRef{{Name: "pv-rbd"}, {Name: "pv-unbound"}, {Name: "sc-rbd", Ignore: "until=2099-01-01"}}},
	}
}

// sortRefs orders each row's refs by name: the order of a list is the
// server's.
func sortRefs(rows []inventory.VolumePluginUse) []inventory.VolumePluginUse {
	for i := range rows {
		slices.SortFunc(rows[i].Objects, func(a, b inventory.ObjectRef) int { return cmp.Compare(a.Name, b.Name) })
	}
	return rows
}

func TestCollectLiveVolumesPersistentVolumesAndStorageClasses(t *testing.T) {
	cs, disc := podFixture(volumeObjects()...)
	servePods(cs, volumeTickPods()...)
	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})
	if got := sortRefs(inv.VolumePlugins); !reflect.DeepEqual(got, wantObjectVolumes()) {
		t.Errorf("VolumePlugins =\n%+v\nwant\n%+v", got, wantObjectVolumes())
	}
	if st := inv.Capabilities[inventory.CapVolumes]; !st.Available || st.Partial {
		t.Errorf("volumes capability = %+v, want available and complete", st)
	}
}

// volumeListServer serves PersistentVolumes and StorageClasses perPage at
// a time (0: all at once), recording each list's options and failing a
// request fail returns an error for.
type volumeListServer struct {
	perPage int
	fail    func(resource string, o metav1.ListOptions) error
	calls   map[string][]metav1.ListOptions
}

func serveVolumeLists(cs *kubefake.Clientset, perPage int, objs ...runtime.Object) *volumeListServer {
	s := &volumeListServer{perPage: perPage, calls: map[string][]metav1.ListOptions{}}
	page := func(o metav1.ListOptions, n int) (start, end int, next string) {
		if o.Continue != "" {
			start, _ = strconv.Atoi(o.Continue)
		}
		end = n
		if s.perPage > 0 && start+s.perPage < n {
			end, next = start+s.perPage, strconv.Itoa(start+s.perPage)
		}
		return start, end, next
	}
	var pvs []corev1.PersistentVolume
	var scs []storagev1.StorageClass
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1.PersistentVolume:
			pvs = append(pvs, *v)
		case *storagev1.StorageClass:
			scs = append(scs, *v)
		}
	}
	for _, resource := range []string{"persistentvolumes", "storageclasses"} {
		cs.PrependReactor("list", resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
			o := a.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
			s.calls[resource] = append(s.calls[resource], o)
			if s.fail != nil {
				if err := s.fail(resource, o); err != nil {
					return true, nil, err
				}
			}
			if resource == "persistentvolumes" {
				start, end, next := page(o, len(pvs))
				return true, &corev1.PersistentVolumeList{ListMeta: metav1.ListMeta{Continue: next}, Items: pvs[start:end]}, nil
			}
			start, end, next := page(o, len(scs))
			return true, &storagev1.StorageClassList{ListMeta: metav1.ListMeta{Continue: next}, Items: scs[start:end]}, nil
		})
	}
	return s
}

// Each is one paged list, of listPageSize objects a page: two pages of
// PersistentVolumes read both, the second from the first's continue token.
func TestCollectLiveVolumesPagesPersistentVolumes(t *testing.T) {
	cs, disc := podFixture()
	servePods(cs, volumeTickPods()...)
	srv := serveVolumeLists(cs, 4, volumeObjects()...)
	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})
	if got := sortRefs(inv.VolumePlugins); !reflect.DeepEqual(got, wantObjectVolumes()) {
		t.Errorf("VolumePlugins =\n%+v\nwant\n%+v", got, wantObjectVolumes())
	}
	pv := srv.calls["persistentvolumes"]
	if len(pv) != 2 || pv[0].Continue != "" || pv[1].Continue != "4" || pv[0].Limit != listPageSize || pv[1].Limit != listPageSize {
		t.Errorf("PersistentVolume lists = %+v, want two pages of limit %d, the second continuing the first", pv, listPageSize)
	}
	if sc := srv.calls["storageclasses"]; len(sc) != 1 || sc[0].Limit != listPageSize {
		t.Errorf("StorageClass lists = %+v, want one page of limit %d", sc, listPageSize)
	}
	if st := inv.Capabilities[inventory.CapVolumes]; !st.Available || st.Partial {
		t.Errorf("volumes capability = %+v, want available and complete", st)
	}
}

// A refused or failed list makes the capability partial, naming what was
// not read; the rest is still counted, and no other capability changes.
func TestCollectLiveVolumesListRefused(t *testing.T) {
	forbidden := func(r string) error {
		g := ""
		if r == "storageclasses" {
			g = "storage.k8s.io"
		}
		return apierrors.NewForbidden(schema.GroupResource{Group: g, Resource: r}, "", errors.New("RBAC"))
	}
	for _, tc := range []struct {
		name     string
		fail     func(resource string, o metav1.ListOptions) error
		reason   string
		skipped  []string
		ebs, rbd int // their counts: pods, and the objects read
	}{
		{"persistentvolumes forbidden", func(r string, _ metav1.ListOptions) error {
			if r == "persistentvolumes" {
				return forbidden(r)
			}
			return nil
		}, "list persistentvolumes: " + forbidden("persistentvolumes").Error() + "; PersistentVolumes were not checked",
			[]string{inventory.SkippedPersistentVolumes}, 1, 1},
		{"storageclasses forbidden", func(r string, _ metav1.ListOptions) error {
			if r == "storageclasses" {
				return forbidden(r)
			}
			return nil
		}, "list storageclasses: " + forbidden("storageclasses").Error() + "; StorageClasses were not checked",
			[]string{inventory.SkippedStorageClasses}, 3, 2},
		{"persistentvolumes fail on the second page", func(r string, o metav1.ListOptions) error {
			if r == "persistentvolumes" && o.Continue != "" {
				return errors.New("etcdserver: request timed out")
			}
			return nil
		}, "list persistentvolumes: etcdserver: request timed out; only the PersistentVolumes listed before the failure were checked",
			[]string{inventory.SkippedPersistentVolumes}, 3, 3},
		{"both forbidden", func(r string, _ metav1.ListOptions) error { return forbidden(r) },
			"list persistentvolumes: " + forbidden("persistentvolumes").Error() + "; PersistentVolumes were not checked; list storageclasses: " + forbidden("storageclasses").Error() + "; StorageClasses were not checked",
			[]string{inventory.SkippedStorageClasses, inventory.SkippedPersistentVolumes}, 1, 0}, // sorted,
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, disc := podFixture()
			servePods(cs, volumeTickPods()...)
			srv := serveVolumeLists(cs, 4, volumeObjects()...)
			srv.fail = tc.fail
			inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})
			st := inv.Capabilities[inventory.CapVolumes]
			if !st.Available || !st.Partial || st.Reason != tc.reason || !reflect.DeepEqual(st.Skipped, tc.skipped) {
				t.Errorf("volumes = %+v\nwant partial, reason %q, skipping %q", st, tc.reason, tc.skipped)
			}
			counts := map[string]int{}
			for _, u := range inv.VolumePlugins {
				counts[u.Plugin] = u.Count
			}
			if counts["awsElasticBlockStore"] != tc.ebs || counts["rbd"] != tc.rbd || counts["glusterfs"] != 2 {
				t.Errorf("counts = %v, want awsElasticBlockStore %d, rbd %d, glusterfs 2 (the pods')", counts, tc.ebs, tc.rbd)
			}
			if a := inv.Capabilities[inventory.CapAddOns]; !a.Available || a.Partial {
				t.Errorf("addons = %+v, want available and complete: the volume lists are not add-on evidence", a)
			}
		})
	}
}

// No pod read: not assessed as before, with no PersistentVolume or
// StorageClass list made for a capability that is not reported.
func TestCollectLiveVolumesNoPodReadListsNoVolumes(t *testing.T) {
	cs, disc := podFixture()
	pods := servePods(cs, volumeTickPods()...)
	pods.failList = func(_ int, ns string, o metav1.ListOptions) error {
		if ns == "" {
			return errors.New("etcdserver: request timed out")
		}
		return nil
	}
	srv := serveVolumeLists(cs, 0, volumeObjects()...)
	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})
	if st := inv.Capabilities[inventory.CapVolumes]; st.Available || !strings.HasPrefix(st.Reason, "list pods: ") || inv.VolumePlugins != nil {
		t.Errorf("volumes = %+v, %+v; want not assessed, nothing recorded", st, inv.VolumePlugins)
	}
	if len(srv.calls) != 0 {
		t.Errorf("lists made %v, want none", srv.calls)
	}
}

// Between full pod passes the PersistentVolumes and StorageClasses are
// still listed every collection: they are never held.
func TestCollectLiveVolumesObjectsListedEveryCollection(t *testing.T) {
	cs, disc := podFixture()
	servePods(cs, volumeTickPods()...)
	srv := serveVolumeLists(cs, 0, volumeObjects()...)
	pass, clk := newPassCache(3, time.Hour)
	for tick := 1; tick <= 3; tick++ {
		inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{PodPass: pass})
		if got := sortRefs(inv.VolumePlugins); !reflect.DeepEqual(got, wantObjectVolumes()) {
			t.Errorf("tick %d: VolumePlugins =\n%+v\nwant\n%+v", tick, got, wantObjectVolumes())
		}
		if pv, sc := len(srv.calls["persistentvolumes"]), len(srv.calls["storageclasses"]); pv != tick || sc != tick {
			t.Errorf("tick %d: %d PersistentVolume and %d StorageClass lists so far, want %d of each", tick, pv, sc, tick)
		}
		clk.t = clk.t.Add(10 * time.Minute)
	}
}

// In files mode a StorageClass manifest with an in-tree provisioner is
// counted for its plugin, located; a CSI one is not.
func TestCollectFilesStorageClassProvisioner(t *testing.T) {
	const docs = `apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: fast}
provisioner: kubernetes.io/gce-pd
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: csi}
provisioner: pd.csi.storage.gke.io
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sc.yaml"), []byte(docs), 0o644); err != nil {
		t.Fatal(err)
	}
	inv, _, err := CollectFiles(dir, loadKB(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []inventory.VolumePluginUse{{Plugin: "gcePersistentDisk", Count: 1, Namespaces: map[string]int{"": 1},
		Objects: []inventory.ObjectRef{{Name: "fast", File: "sc.yaml", Line: 1}}}}
	if !reflect.DeepEqual(inv.VolumePlugins, want) {
		t.Errorf("VolumePlugins =\n%+v\nwant\n%+v", inv.VolumePlugins, want)
	}
}
