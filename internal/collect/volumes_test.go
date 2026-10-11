package collect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// #351: the in-tree volume plugins pods, pod templates and PersistentVolume
// manifests name.

// Every plugin of the dataset is a field the API has: a misspelt one
// would never be found.
func TestVolumePluginNamesAreAPIFields(t *testing.T) {
	fields := map[string]bool{}
	for _, typ := range []reflect.Type{reflect.TypeFor[corev1.VolumeSource](), reflect.TypeFor[corev1.PersistentVolumeSource]()} {
		for i := range typ.NumField() {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			fields[name] = true
		}
	}
	for _, p := range kb.VolumePluginNames() {
		if !fields[p] {
			t.Errorf("plugin %q is no field of corev1.VolumeSource or PersistentVolumeSource", p)
		}
	}
	if _, ok := inTreePluginFields["gitRepo"]; !ok {
		t.Error("gitRepo, a pod volume, is not looked for in pods")
	}
}

// The issue's repro: a Deployment with gitRepo, awsElasticBlockStore and
// glusterfs volumes, a CronJob with a second gitRepo, a PersistentVolume on
// cephfs, and a Deployment with none: each plugin once, located.
const volumeManifests = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  template:
    spec:
      containers: [{name: c, image: busybox}]
      volumes:
      - {name: g, gitRepo: {repository: "https://x"}}
      - {name: e, awsElasticBlockStore: {volumeID: vol-1}}
      - {name: r, glusterfs: {endpoints: x, path: p}}
      - {name: r2, glusterfs: {endpoints: y, path: q}}
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: sync
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers: [{name: c, image: busybox}]
          volumes:
          - {name: g, gitRepo: {repository: "https://y"}}
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: data
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteMany]
  cephfs: {monitors: ["10.0.0.1:6789"]}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: clean
  namespace: shop
spec:
  template:
    spec:
      containers: [{name: c, image: busybox}]
      volumes:
      - {name: t, emptyDir: {}}
`

func wantManifestVolumes(file string) []inventory.VolumePluginUse {
	return []inventory.VolumePluginUse{
		{Plugin: "awsElasticBlockStore", Count: 1, Namespaces: map[string]int{"shop": 1},
			Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "web", File: file, Line: 1}}},
		{Plugin: "cephfs", Count: 1, Namespaces: map[string]int{"": 1},
			Objects: []inventory.ObjectRef{{Name: "data", File: file, Line: 29}}},
		{Plugin: "gitRepo", Count: 2, Namespaces: map[string]int{"shop": 1, "": 1},
			Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "web", File: file, Line: 1}, {Name: "sync", File: file, Line: 16}}},
		{Plugin: "glusterfs", Count: 1, Namespaces: map[string]int{"shop": 1},
			Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "web", File: file, Line: 1}}},
	}
}

func TestCollectFilesVolumePlugins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.yaml"), []byte(volumeManifests), 0o644); err != nil {
		t.Fatal(err)
	}
	inv, _, err := CollectFiles(dir, loadKB(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv.VolumePlugins, wantManifestVolumes("f.yaml")) {
		t.Errorf("VolumePlugins =\n%+v\nwant\n%+v", inv.VolumePlugins, wantManifestVolumes("f.yaml"))
	}
	if st := inv.Capabilities[inventory.CapVolumes]; !st.Available || st.Partial {
		t.Errorf("volumes capability = %+v, want available and complete", st)
	}

	m, err := CollectManifests(strings.NewReader(volumeManifests), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.VolumePlugins, wantManifestVolumes("")) {
		t.Errorf("CollectManifests VolumePlugins =\n%+v\nwant\n%+v", m.VolumePlugins, wantManifestVolumes(""))
	}
	if st := m.Capabilities[inventory.CapVolumes]; !st.Available {
		t.Errorf("CollectManifests volumes capability = %+v, want available", st)
	}
}

// A document that cannot be decoded may hide a volume: the capability says
// so, as partial.
func TestCollectFilesVolumesPartialOnUndecodedDocument(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inv, _, err := CollectFiles(dir, loadKB(t))
	if err != nil {
		t.Fatal(err)
	}
	if st := inv.Capabilities[inventory.CapVolumes]; !st.Available || !st.Partial || !strings.Contains(st.Reason, "1 document(s)") {
		t.Errorf("volumes capability = %+v, want partial naming one document", st)
	}
}

func volumePod(ns, name string, vols ...corev1.Volume) *corev1.Pod {
	p := appPod(name, "busybox")
	p.Namespace = ns
	p.Spec.Volumes = vols
	return p
}

func volumeTickPods() []*corev1.Pod {
	return append(tickPods(),
		volumePod("kube-system", "legacy-sync", corev1.Volume{Name: "g", VolumeSource: corev1.VolumeSource{GitRepo: &corev1.GitRepoVolumeSource{Repository: "https://x"}}}),
		volumePod("shop", "web-1",
			corev1.Volume{Name: "r", VolumeSource: corev1.VolumeSource{Glusterfs: &corev1.GlusterfsVolumeSource{EndpointsName: "x", Path: "p"}}},
			corev1.Volume{Name: "r2", VolumeSource: corev1.VolumeSource{Glusterfs: &corev1.GlusterfsVolumeSource{EndpointsName: "y", Path: "q"}}},
			corev1.Volume{Name: "e", VolumeSource: corev1.VolumeSource{AWSElasticBlockStore: &corev1.AWSElasticBlockStoreVolumeSource{VolumeID: "vol-1"}}}),
		volumePod("shop", "web-2", corev1.Volume{Name: "r", VolumeSource: corev1.VolumeSource{Glusterfs: &corev1.GlusterfsVolumeSource{EndpointsName: "x", Path: "p"}}}),
		volumePod("shop", "clean", corev1.Volume{Name: "t", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}),
	)
}

func wantLiveVolumes() []inventory.VolumePluginUse {
	return []inventory.VolumePluginUse{
		{Plugin: "awsElasticBlockStore", Count: 1, Namespaces: map[string]int{"shop": 1}},
		{Plugin: "gitRepo", Count: 1, Namespaces: map[string]int{"kube-system": 1}},
		{Plugin: "glusterfs", Count: 2, Namespaces: map[string]int{"shop": 2}},
	}
}

// Live, the volume plugins of pods come from the pod lists the versions
// and add-ons steps make anyway: the same pod requests as a cluster without
// them, each pod sent once. The capability's own requests are exactly two:
// one list of PersistentVolumes and one of StorageClasses, each one page
// here (#362).
func TestCollectLiveVolumesTwoExtraRequests(t *testing.T) {
	for _, perPage := range []int{0, 1} {
		requests := func(pods []*corev1.Pod) (inventory.Inventory, *podServer, map[string]int) {
			cs, disc := podFixture()
			srv := servePods(cs, pods...)
			srv.perPage = perPage
			inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})
			byResource := map[string]int{}
			for _, a := range cs.Actions() {
				byResource[a.GetVerb()+" "+a.GetResource().Resource]++
			}
			return inv, srv, byResource
		}
		plain := append(tickPods(), volumePod("shop", "web-1"), volumePod("shop", "web-2"), volumePod("shop", "clean"), volumePod("kube-system", "legacy-sync"))
		_, plainSrv, plainActions := requests(plain)
		inv, srv, actions := requests(volumeTickPods())

		if !reflect.DeepEqual(actions, plainActions) || !reflect.DeepEqual(srv.calls, plainSrv.calls) {
			t.Errorf("perPage %d: requests %v (pod lists %+v), want the %v of the same pods without in-tree volumes (%+v)",
				perPage, actions, srv.calls, plainActions, plainSrv.calls)
		}
		var own []string
		for r, n := range actions {
			if strings.HasSuffix(r, " persistentvolumes") || strings.HasSuffix(r, " storageclasses") {
				own = append(own, fmt.Sprintf("%s x%d", r, n))
			}
		}
		slices.Sort(own)
		if want := []string{"list persistentvolumes x1", "list storageclasses x1"}; !slices.Equal(own, want) {
			t.Errorf("perPage %d: the capability's own requests = %v, want exactly %v", perPage, own, want)
		}
		for pod, n := range srv.served {
			if n != 1 {
				t.Errorf("perPage %d: pod %s sent %d times, want once", perPage, pod, n)
			}
		}
		if !reflect.DeepEqual(inv.VolumePlugins, wantLiveVolumes()) {
			t.Errorf("perPage %d: VolumePlugins =\n%+v\nwant\n%+v", perPage, inv.VolumePlugins, wantLiveVolumes())
		}
		if st := inv.Capabilities[inventory.CapVolumes]; !st.Available || st.Partial {
			t.Errorf("perPage %d: volumes capability = %+v, want available and complete", perPage, st)
		}
	}
}

// Between full pod passes the pods outside kube-system are reported as the
// held pass read them, with no list of them; kube-system's are this tick's.
func TestCollectLiveVolumesFromHeldPass(t *testing.T) {
	cs, disc := podFixture()
	srv := servePods(cs, volumeTickPods()...)
	pass, clk := newPassCache(3, time.Hour)
	clients := Clients{Kube: cs, Discovery: disc}
	for tick := 1; tick <= 3; tick++ {
		srv.calls = nil
		inv := Collect(context.Background(), clients, loadKB(t), Options{PodPass: pass})
		if got, want := srv.allNamespaceLists(), map[bool]int{true: 1, false: 0}[tick == 1]; got != want {
			t.Errorf("tick %d: %d lists of the other namespaces, want %d", tick, got, want)
		}
		if !reflect.DeepEqual(inv.VolumePlugins, wantLiveVolumes()) {
			t.Errorf("tick %d: VolumePlugins =\n%+v\nwant\n%+v", tick, inv.VolumePlugins, wantLiveVolumes())
		}
		if st := inv.Capabilities[inventory.CapVolumes]; !st.Available || st.Partial {
			t.Errorf("tick %d: volumes capability = %+v, want available and complete", tick, st)
		}
		clk.t = clk.t.Add(10 * time.Minute)
	}
}

// A pod list that fails on its first page leaves the volumes not assessed;
// on a later page, partial with the pages read.
func TestCollectLiveVolumesPodListFails(t *testing.T) {
	for _, later := range []bool{false, true} {
		cs, disc := podFixture()
		srv := servePods(cs, volumeTickPods()...)
		srv.perPage = 1
		srv.failList = func(_ int, ns string, o metav1.ListOptions) error {
			if ns == "" && (o.Continue != "") == later {
				return errors.New("etcdserver: request timed out")
			}
			return nil
		}
		inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})
		st := inv.Capabilities[inventory.CapVolumes]
		switch {
		case !later && (st.Available || len(inv.VolumePlugins) != 0 || !strings.Contains(st.Reason, "list pods: ")):
			t.Errorf("first page fails: volumes = %+v, %+v; want not assessed, nothing recorded", st, inv.VolumePlugins)
		case later && (!st.Available || !st.Partial || !reflect.DeepEqual(st.Skipped, []string{inventory.SkippedPods})):
			t.Errorf("later page fails: volumes = %+v; want partial, skipping the pods", st)
		}
	}
}
