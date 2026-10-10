package collect

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// collectCRDs reads every CustomResourceDefinition through
// apiextensions.k8s.io/v1, paged (RBAC: customresourcedefinitions list,
// which the chart's KB rules grant), and records its versions and
// status.storedVersions.
//
// For a CRD with a version it deprecates or marks served: false, its
// custom resources are listed once, metadata-only and paged, at a served
// version that is not deprecated (the storage version when it is one):
// listing the deprecated version would make the scanner one of its
// callers. An object counts for such a version when some manager still
// writes it through it (authoringManager, as api-usage judges built-in
// APIs); objects nobody writes through it are only converted on read, and
// do not count. A version dropped from spec.versions altogether is not
// looked for: nothing in a live CRD names it.
//
// Reading CRDs failing degrades the capability. A custom-resource LIST
// failing (agents are granted no custom resources), or a CRD that serves
// no version to list at, leaves the CRDs read: the capability comes back
// partial, naming as Skipped each "group/version Kind" whose use went
// unchecked.
func collectCRDs(ctx context.Context, ext apiextensionsclient.Interface, meta metadata.Interface, inv *inventory.Inventory) error {
	var crds []inventory.CRD
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		page, err := ext.ApiextensionsV1().CustomResourceDefinitions().List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list apiextensions.k8s.io/v1 customresourcedefinitions: %w", err)
		}
		for i := range page.Items {
			crds = append(crds, crdOf(&page.Items[i]))
		}
		if page.Continue == "" {
			break
		}
		opts.Continue = page.Continue
	}
	slices.SortFunc(crds, func(a, b inventory.CRD) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(a.Kind, b.Kind))
	})

	var failures []string
	unchecked := map[string]bool{}
	for i := range crds {
		c := &crds[i]
		var look []string // versions whose use is looked for
		for _, v := range c.Versions {
			if v.Deprecated && v.Served || !v.Served {
				look = append(look, v.Name)
			}
		}
		if len(look) == 0 {
			continue
		}
		skip := func() {
			for _, v := range look {
				unchecked[apiName(c.Group+"/"+v, c.Kind)] = true
			}
		}
		at := c.PreferredVersion()
		if at == "" {
			failures = append(failures, fmt.Sprintf("%s.%s serves no version that is not deprecated, so its custom resources were not listed", c.Plural, c.Group))
			skip()
			continue
		}
		gvr := schema.GroupVersionResource{Group: c.Group, Version: at, Resource: c.Plural}
		usage, err := listCRDUsage(ctx, meta, gvr, c.Kind, look)
		if err != nil {
			failures = append(failures, fmt.Sprintf("list %s %s: %v", gvr.GroupVersion(), gvr.Resource, err))
			skip()
			continue
		}
		c.Usage = usage
	}
	inv.CRDs = crds
	if len(failures) == 0 {
		return nil
	}
	return partialError{msg: strings.Join(failures, "; "), incomplete: true, skipped: slices.Sorted(maps.Keys(unchecked))}
}

// crdOf records what the engine judges of a CRD.
func crdOf(o *apiextensionsv1.CustomResourceDefinition) inventory.CRD {
	c := inventory.CRD{
		Group: o.Spec.Group, Kind: o.Spec.Names.Kind, Plural: o.Spec.Names.Plural,
		StoredVersions: slices.Clone(o.Status.StoredVersions),
	}
	for _, v := range o.Spec.Versions {
		cv := inventory.CRDVersion{Name: v.Name, Served: v.Served, Storage: v.Storage, Deprecated: v.Deprecated}
		if v.DeprecationWarning != nil {
			cv.DeprecationWarning = *v.DeprecationWarning
		}
		c.Versions = append(c.Versions, cv)
	}
	return c
}

// listCRDUsage pages through one CRD's custom resources, metadata-only,
// and returns, per version of look that some manager still writes them
// through, their usage, sorted by version.
func listCRDUsage(ctx context.Context, meta metadata.Interface, gvr schema.GroupVersionResource, kind string, look []string) ([]inventory.APIUsage, error) {
	byVersion := map[string]*inventory.APIUsage{}
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		page, err := meta.Resource(gvr).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for i := range page.Items {
			m := &page.Items[i]
			for _, v := range look {
				manager := authoringManager(m, gvr.Group+"/"+v)
				if manager == "" {
					continue
				}
				u := byVersion[v]
				if u == nil {
					u = &inventory.APIUsage{Group: gvr.Group, Version: v, Kind: kind, Namespaces: map[string]int{}}
					byVersion[v] = u
				}
				u.Count++
				u.Namespaces[m.Namespace]++
				if len(u.Objects) < inventory.MaxObjectRefs {
					u.Objects = append(u.Objects, withIgnore(inventory.ObjectRef{
						Namespace: m.Namespace, Name: m.Name, Manager: manager,
					}, m.Annotations))
				} else {
					u.ObjectsOmitted++
				}
			}
		}
		if page.Continue == "" {
			break
		}
		opts.Continue = page.Continue
	}
	var out []inventory.APIUsage
	for _, v := range slices.Sorted(maps.Keys(byVersion)) {
		out = append(out, *byVersion[v])
	}
	return out, nil
}
