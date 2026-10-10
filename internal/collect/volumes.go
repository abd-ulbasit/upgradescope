package collect

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// The volumes capability (#351): the in-tree volume plugins
// (kb.VolumePluginNames) that pods, pod templates and PersistentVolume
// manifests name. Live, it reads the pods the add-ons step lists anyway,
// and the kube-system pods the versions step lists, at no request of its
// own; between full pod passes it takes the pods outside kube-system from
// the held pass (PodPassCache), as the add-ons do. It also makes two paged
// lists of its own every collection, never held (#362): the
// PersistentVolumes, by their in-tree source, and the StorageClasses, by an
// in-tree provisioner (kb.VolumePluginProvisioners).

// pluginFields maps each plugin name to its field's index in the struct
// type T, by the field's JSON name. A plugin with no field in T has no
// entry.
func pluginFields[T any]() map[string]int {
	byJSON := map[string]int{}
	t := reflect.TypeFor[T]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		byJSON[name] = i
	}
	out := map[string]int{}
	for _, p := range kb.VolumePluginNames() {
		if i, ok := byJSON[p]; ok {
			out[p] = i
		}
	}
	return out
}

// inTreePluginFields are the plugins' fields in corev1.VolumeSource, the
// type of a pod's volumes; pvPluginFields theirs in
// corev1.PersistentVolumeSource, so a plugin only a PersistentVolume can
// name is found there.
var (
	inTreePluginFields = pluginFields[corev1.VolumeSource]()
	pvPluginFields     = pluginFields[corev1.PersistentVolumeSource]()
)

// setPlugins returns the plugins of fields whose pointer field in v (a
// struct of the type fields was built for) is set, appended to out once
// each.
func setPlugins(out []string, v reflect.Value, fields map[string]int) []string {
	for p, f := range fields {
		if !v.Field(f).IsNil() && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// podVolumePlugins returns the in-tree plugins vols name, sorted, each
// once.
func podVolumePlugins(vols []corev1.Volume) []string {
	var out []string
	for i := range vols {
		out = setPlugins(out, reflect.ValueOf(&vols[i].VolumeSource).Elem(), inTreePluginFields)
	}
	slices.Sort(out)
	return out
}

// pvVolumePlugins returns the in-tree plugins src names, sorted (the API
// allows one).
func pvVolumePlugins(src *corev1.PersistentVolumeSource) []string {
	out := setPlugins(nil, reflect.ValueOf(src).Elem(), pvPluginFields)
	slices.Sort(out)
	return out
}

// volumeProvisioners maps the in-tree StorageClass provisioners to their
// plugins.
var volumeProvisioners = kb.VolumePluginProvisioners()

var (
	persistentVolumeGK = schema.GroupKind{Kind: "PersistentVolume"}
	storageClassGK     = schema.GroupKind{Group: "storage.k8s.io", Kind: "StorageClass"}
)

// manifestVolumePlugins returns the in-tree plugins a manifest object of
// kind gk names: a workload's pod template volumes (podTemplatePaths), a
// Pod's own, a PersistentVolume's spec, or a StorageClass's in-tree
// provisioner; sorted, each once, nil when it names none. Values that are
// not what the API takes (an unrendered template) are skipped.
func manifestVolumePlugins(gk schema.GroupKind, obj map[string]any) []string {
	var sources []map[string]any
	if gk == storageClassGK {
		if prov, ok := obj["provisioner"].(string); ok && volumeProvisioners[prov] != "" {
			return []string{volumeProvisioners[prov]}
		}
		return nil
	}
	if gk == persistentVolumeGK {
		spec, _, _ := unstructured.NestedFieldNoCopy(obj, "spec")
		if m, ok := spec.(map[string]any); ok {
			sources = append(sources, m)
		}
	} else if path, ok := podTemplatePaths[gk]; ok {
		vols, _, _ := unstructured.NestedFieldNoCopy(obj, append(slices.Clip(path), "spec", "volumes")...)
		list, _ := vols.([]any)
		for _, v := range list {
			if m, ok := v.(map[string]any); ok {
				sources = append(sources, m)
			}
		}
	}
	var out []string
	for _, src := range sources {
		for _, p := range kb.VolumePluginNames() {
			if v, ok := src[p]; ok && v != nil && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return out
}

// attachVolumes copies the plugins kubectl's decoder read of each object
// onto the walk's object of the same group/version/kind, namespace and
// name, in stream order: the walk reads no spec, and kubectl's objects
// carry no lines.
func attachVolumes(objs, kubectl []manifestObject) {
	type id struct {
		gvk
		namespace, name string
	}
	queues := map[id][][]string{}
	for _, o := range kubectl {
		k := id{gvk{o.group, o.version, o.kind}, o.ref.Namespace, o.ref.Name}
		queues[k] = append(queues[k], o.volumes)
	}
	for i := range objs {
		k := id{gvk{objs[i].group, objs[i].version, objs[i].kind}, objs[i].ref.Namespace, objs[i].ref.Name}
		if q := queues[k]; len(q) > 0 {
			objs[i].volumes, queues[k] = q[0], q[1:]
		}
	}
}

// volumeTally counts the users of each in-tree plugin: pods by namespace
// and PersistentVolumes and StorageClasses with their refs (live), or
// manifest objects by namespace with their refs (files). Its zero value is
// empty and ready to use.
type volumeTally struct {
	byPlugin map[string]*inventory.VolumePluginUse
}

func (t *volumeTally) add(plugin, namespace string, n int, ref *inventory.ObjectRef) {
	if t.byPlugin == nil {
		t.byPlugin = map[string]*inventory.VolumePluginUse{}
	}
	u := t.byPlugin[plugin]
	if u == nil {
		u = &inventory.VolumePluginUse{Plugin: plugin, Namespaces: map[string]int{}}
		t.byPlugin[plugin] = u
	}
	u.Count += n
	u.Namespaces[namespace] += n
	if ref != nil {
		if len(u.Objects) < inventory.MaxObjectRefs {
			u.Objects = append(u.Objects, *ref)
		} else {
			u.ObjectsOmitted++
		}
	}
}

// addPod counts one live pod for each plugin its volumes name.
func (t *volumeTally) addPod(namespace string, vols []corev1.Volume) {
	for _, p := range podVolumePlugins(vols) {
		t.add(p, namespace, 1, nil)
	}
}

// addObjects counts manifest objects, with their refs.
func (t *volumeTally) addObjects(objs []manifestObject) {
	for i := range objs {
		for _, p := range objs[i].volumes {
			t.add(p, objs[i].ref.Namespace, 1, &objs[i].ref)
		}
	}
}

// merge adds o's live pod counts to t.
func (t *volumeTally) merge(o volumeTally) {
	for _, p := range slices.Sorted(maps.Keys(o.byPlugin)) {
		for ns, n := range o.byPlugin[p].Namespaces {
			t.add(p, ns, n, nil)
		}
	}
}

// without returns t's live pod counts outside namespace ns: what a pod
// pass holds (PodPassCache.record), the kube-system pods being read every
// collection.
func (t volumeTally) without(ns string) volumeTally {
	var out volumeTally
	for p, u := range t.byPlugin {
		for k, n := range u.Namespaces {
			if k != ns {
				out.add(p, k, n, nil)
			}
		}
	}
	return out
}

// rows returns the tally as Inventory.VolumePlugins, sorted by plugin.
func (t volumeTally) rows() []inventory.VolumePluginUse {
	var out []inventory.VolumePluginUse
	for _, p := range slices.Sorted(maps.Keys(t.byPlugin)) {
		out = append(out, *t.byPlugin[p])
	}
	return out
}

// volumeObjectsRead says what the PersistentVolume and StorageClass lists
// of a collection did not read: the reasons, and the Skipped entries.
type volumeObjectsRead struct {
	reasons, skipped []string
}

// listVolumeObjects adds the in-tree plugins of the cluster's
// PersistentVolumes and StorageClasses to t: one paged list of each, of
// listPageSize objects a page. A PersistentVolume is counted under its
// claim's namespace when it is bound, and under none otherwise; a
// StorageClass under none. Each is named in the plugin's objects, with its
// ignore annotations. A list that fails keeps the pages read before it.
func listVolumeObjects(ctx context.Context, kube kubernetes.Interface, t *volumeTally) volumeObjectsRead {
	var out volumeObjectsRead
	failed := func(resource, kind, skipped string, cont string, err error) {
		what := kind + " were not checked"
		if cont != "" {
			what = "only the " + kind + " listed before the failure were checked"
		}
		out.reasons = append(out.reasons, fmt.Sprintf("list %s: %v; %s", resource, err, what))
		out.skipped = append(out.skipped, skipped)
	}
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		pvs, err := kube.CoreV1().PersistentVolumes().List(ctx, opts)
		if err != nil {
			failed("persistentvolumes", "PersistentVolumes", inventory.SkippedPersistentVolumes, opts.Continue, err)
			break
		}
		for i := range pvs.Items {
			pv := &pvs.Items[i]
			ns := ""
			if pv.Spec.ClaimRef != nil && pv.Status.Phase == corev1.VolumeBound {
				ns = pv.Spec.ClaimRef.Namespace
			}
			for _, p := range pvVolumePlugins(&pv.Spec.PersistentVolumeSource) {
				ref := withIgnore(inventory.ObjectRef{Name: pv.Name}, pv.Annotations)
				t.add(p, ns, 1, &ref)
			}
		}
		if pvs.Continue == "" {
			break
		}
		opts.Continue = pvs.Continue
	}
	opts = metav1.ListOptions{Limit: listPageSize}
	for {
		classes, err := kube.StorageV1().StorageClasses().List(ctx, opts)
		if err != nil {
			failed("storageclasses", "StorageClasses", inventory.SkippedStorageClasses, opts.Continue, err)
			break
		}
		for i := range classes.Items {
			sc := &classes.Items[i]
			if p := volumeProvisioners[sc.Provisioner]; p != "" {
				ref := withIgnore(inventory.ObjectRef{Name: sc.Name}, sc.Annotations)
				t.add(p, "", 1, &ref)
			}
		}
		if classes.Continue == "" {
			break
		}
		opts.Continue = classes.Continue
	}
	return out
}

// setLiveVolumes records the volume plugins the add-ons step's pod lists
// and the PersistentVolume and StorageClass lists found, and the volumes
// capability: available when every pod was read (this collection, or the
// held pass for the pods outside kube-system) and both lists were;
// partial when a later page of the pods failed or a list was refused or
// failed, naming what was not read; not assessed when no pod was read.
func setLiveVolumes(inv *inventory.Inventory, t volumeTally, podErr error, podsRead bool, objs volumeObjectsRead) {
	if inv.Capabilities == nil {
		inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{}
	}
	inv.VolumePlugins = t.rows()
	st := inventory.CapabilityStatus{Available: true}
	switch {
	case podErr == nil:
	case podsRead:
		st.Reason = fmt.Sprintf("list pods: %v; volume plugins were read from the pods listed before the failure only", podErr)
		st.Skipped = []string{inventory.SkippedPods}
	default:
		inv.VolumePlugins = nil
		inv.Capabilities[inventory.CapVolumes] = inventory.CapabilityStatus{Reason: fmt.Sprintf("list pods: %v", podErr)}
		return
	}
	if len(objs.reasons) > 0 {
		st.Reason = strings.Join(slices.DeleteFunc(append([]string{st.Reason}, objs.reasons...), func(s string) bool { return s == "" }), "; ")
		st.Skipped = slices.Sorted(slices.Values(append(st.Skipped, objs.skipped...)))
	}
	st.Partial = st.Reason != ""
	inv.Capabilities[inventory.CapVolumes] = st
}

// assessVolumes records the volume plugins of manifest objects, and the
// volumes capability: partial when undecoded documents (unassessed of
// them) may hide some.
func assessVolumes(inv *inventory.Inventory, t volumeTally, unassessed int) {
	inv.VolumePlugins = t.rows()
	st := inventory.CapabilityStatus{Available: true}
	if unassessed > 0 {
		st.Partial = true
		st.Reason = fmt.Sprintf("%d document(s) could not be decoded, so the volumes in them were not checked", unassessed)
	}
	inv.Capabilities[inventory.CapVolumes] = st
}
