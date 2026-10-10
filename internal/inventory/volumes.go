package inventory

import "fmt"

// VolumePluginUse is one in-tree volume plugin in use (#351): the pods
// (live) or the pod templates, pods and PersistentVolumes (manifests)
// whose volumes name it, by its field name in corev1.VolumeSource or
// corev1.PersistentVolumeSource ("glusterfs", "awsElasticBlockStore").
//
// Count is how many name it, each once whatever the number of its volumes
// that do. Namespaces counts them per namespace ("" for a PersistentVolume,
// which is cluster-scoped, and for a manifest object without
// metadata.namespace). Objects locates them in manifests only (File, Line,
// and the workload's or PersistentVolume's Namespace and Name), at most
// MaxObjectRefs, ObjectsOmitted counting the rest; a live collection
// names no pod, so its size does not grow with the pods. Between full pod
// passes, the agent reports the pods outside kube-system as the last full
// pass read them, for at most Inventory.AddOnEvidenceAgeSeconds.
type VolumePluginUse struct {
	Plugin         string         `json:"plugin"`
	Count          int            `json:"count"`
	Namespaces     map[string]int `json:"namespaces,omitempty"`
	Objects        []ObjectRef    `json:"objects,omitempty"`
	ObjectsOmitted int            `json:"objectsOmitted,omitempty"`
}

// MaxVolumePlugins caps Inventory.VolumePlugins: one entry per plugin, of
// the few dozen fields corev1.VolumeSource has.
const MaxVolumePlugins = 64

// asUsage is v as validateUsage checks an API usage entry: its namespace
// keys and object refs are the same identifiers.
func (v VolumePluginUse) asUsage() APIUsage {
	return APIUsage{Kind: v.Plugin, Count: v.Count, Namespaces: v.Namespaces, Objects: v.Objects, ObjectsOmitted: v.ObjectsOmitted}
}

// validateVolumeIdentifiers is ValidateIdentifiers for VolumePlugins.
func (inv Inventory) validateVolumeIdentifiers() error {
	for i, v := range inv.VolumePlugins {
		if err := validateUsage(func() string { return fmt.Sprintf("volumePlugins[%d]", i) }, v.asUsage()); err != nil {
			return err
		}
	}
	return nil
}

// validateVolumeLimits is ValidateLimits for VolumePlugins: at most
// MaxVolumePlugins entries, each plugin named once and non-empty, and at
// most MaxObjectRefs objects each.
func (inv Inventory) validateVolumeLimits() error {
	if n := len(inv.VolumePlugins); n > MaxVolumePlugins {
		p := fmt.Sprintf("%d plugins, over the %d a collector lists", n, MaxVolumePlugins)
		return &LimitError{Field: "volumePlugins", Problem: p, At: "volumePlugins", Limit: p}
	}
	seen := map[string]bool{}
	for i, v := range inv.VolumePlugins {
		at := fmt.Sprintf("volumePlugins[%d]", i)
		switch {
		case v.Plugin == "":
			p := "no plugin named"
			return &LimitError{Field: at + ".plugin", Problem: p, At: at + ".plugin", Limit: p}
		case seen[v.Plugin]:
			p := fmt.Sprintf("plugin %s listed twice, where a collector counts each once", quoteShort(v.Plugin))
			return &LimitError{Field: at + ".plugin", Problem: p, At: at + ".plugin", Limit: "a plugin listed twice, where a collector counts each once"}
		case len(v.Objects) > MaxObjectRefs:
			p := fmt.Sprintf("%d objects, over the %d a collector lists (it counts the rest in objectsOmitted)", len(v.Objects), MaxObjectRefs)
			return &LimitError{Field: at + ".objects", Problem: p, At: at + ".objects", Limit: p}
		}
		seen[v.Plugin] = true
	}
	return nil
}
