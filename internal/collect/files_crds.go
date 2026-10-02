package collect

import (
	"fmt"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// crdGVK is the CustomResourceDefinition API files mode reads.
var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// notCustomResources are API groups of files that carry apiVersion and
// kind but are never sent to a cluster, so no CRD is expected for them.
// Groups without a dot (skaffold/v4beta6 Config) need no entry: a CRD's
// group must contain one, so no CRD can define them (customResource).
var notCustomResources = map[string]bool{
	"kustomize.config.k8s.io": true, // kustomization.yaml
	"kpt.dev":                 true, // Kptfile
}

// customResource reports whether objects of group can only be served
// through a CRD: the group is not built in, not tool configuration, and
// is a domain with a dot, as the apiserver requires of a CRD's group.
func customResource(group string) bool {
	return strings.Contains(group, ".") && !builtinGroup(group) && !notCustomResources[group]
}

// manifestCRD reads a CustomResourceDefinition manifest as the live
// collector reads a CRD (crdOf), without status: a manifest's
// status.storedVersions says nothing about a cluster. One that does not
// convert is returned empty.
func manifestCRD(u *unstructured.Unstructured) *inventory.CRD {
	var o apiextensionsv1.CustomResourceDefinition
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o); err != nil {
		return &inventory.CRD{}
	}
	c := crdOf(&o)
	c.StoredVersions = nil
	return &c
}

// attachCRDs copies what kubectl's decoder read of each CRD onto the
// walk's CRD objects, in order: the walk reads no spec. The two found the
// same objects (gvkDiff), so the CRDs pair up in stream order.
func attachCRDs(objs, kubectl []manifestObject) {
	var defs []*inventory.CRD
	for _, o := range kubectl {
		if o.crd != nil {
			defs = append(defs, o.crd)
		}
	}
	for i := range objs {
		if len(defs) == 0 {
			return
		}
		if (schema.GroupVersionKind{Group: objs[i].group, Version: objs[i].version, Kind: objs[i].kind}) == crdGVK {
			objs[i].crd, defs = defs[0], defs[1:]
		}
	}
}

// crdsOf returns the CRDs objs define, in order.
func crdsOf(objs []manifestObject) []inventory.CRD {
	var out []inventory.CRD
	for _, o := range objs {
		if o.crd != nil && o.crd.Group != "" && o.crd.Kind != "" {
			out = append(out, *o.crd)
		}
	}
	return out
}

// assessCRDs records the CRDs found in manifests (crds, in walk order; a
// later definition of the same group and kind replaces an earlier one, as
// a later kubectl apply does) and, as each one's Usage, the manifest
// objects of its kind at a version it deprecates, does not serve or does
// not list, from inv.APIUsage (every manifest object). Custom resources
// with no CRD in the manifests (customResource) could not be judged:
// the crds capability is then partial, naming them.
func assessCRDs(inv *inventory.Inventory, crds []inventory.CRD) {
	byKind := map[schema.GroupKind]int{}
	var out []inventory.CRD
	for _, c := range crds {
		gk := schema.GroupKind{Group: c.Group, Kind: c.Kind}
		if i, ok := byKind[gk]; ok {
			out[i] = c
			continue
		}
		byKind[gk] = len(out)
		out = append(out, c)
	}
	var orphans []string
	for _, u := range inv.APIUsage {
		i, ok := byKind[schema.GroupKind{Group: u.Group, Kind: u.Kind}]
		if !ok {
			if customResource(u.Group) {
				orphans = append(orphans, apiName(u.Group+"/"+u.Version, u.Kind))
			}
			continue
		}
		if j := slices.IndexFunc(out[i].Versions, func(v inventory.CRDVersion) bool { return v.Name == u.Version }); j >= 0 {
			if v := out[i].Versions[j]; v.Served && !v.Deprecated {
				continue
			}
		}
		out[i].Usage = append(out[i].Usage, u)
	}
	slices.SortFunc(out, func(a, b inventory.CRD) int {
		if a.Group != b.Group {
			return strings.Compare(a.Group, b.Group)
		}
		return strings.Compare(a.Kind, b.Kind)
	})
	inv.CRDs = out
	if len(orphans) == 0 {
		inv.Capabilities[inventory.CapCRDs] = inventory.CapabilityStatus{Available: true}
		return
	}
	slices.Sort(orphans)
	reason := fmt.Sprintf("no CRD in the scanned files for %s, so whether its version is served was not assessed", orphans[0])
	if len(orphans) > 1 {
		listed := orphans[:min(len(orphans), 5)]
		more := ""
		if n := len(orphans) - len(listed); n > 0 {
			more = fmt.Sprintf(" and %d more", n)
		}
		reason = fmt.Sprintf("no CRD in the scanned files for %d custom resource APIs (%s%s), so whether their versions are served was not assessed",
			len(orphans), strings.Join(listed, ", "), more)
	}
	inv.Capabilities[inventory.CapCRDs] = inventory.CapabilityStatus{Available: true, Partial: true, Reason: reason, Skipped: orphans}
}

// builtinGroup reports whether group is served by Kubernetes itself (the
// API groups client-go knows, and the two the apiserver adds), so no CRD
// defines its objects.
func builtinGroup(group string) bool {
	return group == "apiextensions.k8s.io" || group == "apiregistration.k8s.io" || scheme.Scheme.IsGroupRegistered(group)
}
