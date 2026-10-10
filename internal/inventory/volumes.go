package inventory

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

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

// ruleVolumePlugin is what a plugin name must be: the JSON name of a
// corev1.VolumeSource or PersistentVolumeSource field.
const ruleVolumePlugin = "a volume plugin: a field name of at most 64 ASCII letters and digits, starting with a letter"

var volumePluginName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]{0,63}$`)

// volumePluginProblems says why s is not a volume plugin's field name.
func volumePluginProblems(s string) []string {
	if volumePluginName.MatchString(s) {
		return nil
	}
	if len(s) > 64 {
		return []string{fmt.Sprintf("longer than 64 bytes (%d)", len(s))}
	}
	return []string{"not a field name: ASCII letters and digits, starting with a letter"}
}

// validateVolumeIdentifiers is ValidateIdentifiers for VolumePlugins: each
// plugin is a field name, and its namespace keys and object refs are the
// identifiers an API usage entry's are.
func (inv Inventory) validateVolumeIdentifiers() error {
	for i, v := range inv.VolumePlugins {
		at := func() string { return fmt.Sprintf("volumePlugins[%d]", i) }
		if p := problemsWith(v.Plugin, volumePluginProblems); p != nil {
			return &IdentifierError{Field: at() + ".plugin", Value: v.Plugin, Rule: ruleVolumePlugin, Problems: p}
		}
		if err := validateUsage(at, v.asUsage()); err != nil {
			return err
		}
	}
	return nil
}

// validateVolumeLimits is ValidateLimits for VolumePlugins: at most
// MaxVolumePlugins entries, each plugin named once and non-empty, counts
// that are not negative, at most MaxObjectRefs objects each, and object
// managers the apiserver would accept.
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
		case v.Count < 0 || v.ObjectsOmitted < 0:
			p := "a negative count, where a collector counts what it saw"
			return &LimitError{Field: at, Problem: p, At: at, Limit: p}
		case len(v.Objects) > MaxObjectRefs:
			p := fmt.Sprintf("%d objects, over the %d a collector lists (it counts the rest in objectsOmitted)", len(v.Objects), MaxObjectRefs)
			return &LimitError{Field: at + ".objects", Problem: p, At: at + ".objects", Limit: p}
		}
		// Of several negative namespace counts the least key is named, so
		// the message does not depend on map order.
		bad, found := "", false
		for ns, n := range v.Namespaces {
			if n < 0 && (!found || ns < bad) {
				bad, found = ns, true
			}
		}
		if found {
			p := fmt.Sprintf("namespace %s counted negative, where a collector counts what it saw", quoteShort(bad))
			return &LimitError{Field: at + ".namespaces", Problem: p, At: at + ".namespaces", Limit: "a namespace counted negative, where a collector counts what it saw"}
		}
		for k, o := range v.Objects {
			if p, limit := managerProblem(o.Manager); p != "" {
				field := fmt.Sprintf("%s.objects[%d].manager", at, k)
				return &LimitError{Field: field, Problem: p, At: field, Limit: limit}
			}
		}
		seen[v.Plugin] = true
	}
	return nil
}

// conformVolumes repairs VolumePlugins in place, as conformUsages repairs
// an API usage entry: the namespace keys that are not namespace names and
// the objects Admit would refuse or beyond MaxObjectRefs are dropped (the
// objects counted in objectsOmitted), and the entries beyond
// MaxVolumePlugins are dropped, named in volumes' Skipped. An entry whose
// plugin is not a field name, or whose counts are negative, is left to
// dropElement, which drops it whole.
func (inv *Inventory) conformVolumes(r *repairs) {
	for i := range inv.VolumePlugins {
		v := &inv.VolumePlugins[i]
		v.Objects = conformRefs(v.Namespaces, v.Objects, &v.ObjectsOmitted, CapVolumes, "volume plugin use", r)
	}
	if len(inv.VolumePlugins) > MaxVolumePlugins {
		for _, v := range inv.VolumePlugins[MaxVolumePlugins:] {
			r.add(CapVolumes, "volume plugin entr(ies) beyond the cap dropped", volumeLabel(v))
		}
		clear(inv.VolumePlugins[MaxVolumePlugins:])
		inv.VolumePlugins = slices.Clip(inv.VolumePlugins[:MaxVolumePlugins])
	}
}

// volumeLabel names a volume plugin entry in volumes' Skipped: the plugin,
// or, where it is not a field name, quoted and cut.
func volumeLabel(v VolumePluginUse) string {
	if volumePluginProblems(v.Plugin) == nil {
		return v.Plugin
	}
	return quoteShort(utf8Prefix(v.Plugin, 40))
}

// article is "an" before a word starting with a vowel, else "a".
func article(word string) string {
	if word != "" && strings.ContainsRune("AEIOUaeiou", rune(word[0])) {
		return "an"
	}
	return "a"
}
