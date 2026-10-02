package collect

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

const (
	helmSecretType = "helm.sh/release.v1"
	// helmObjectPrefix starts the name of every object Helm v3 stores a
	// revision in: "sh.helm.release.v1.<release>.v<revision>".
	helmObjectPrefix = "sh.helm.release.v1."
)

// helmReleaseDoc is the minimal slice of Helm's release JSON we decode.
// No Helm SDK dependency.
type helmReleaseDoc struct {
	Info struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name        string `json:"name"`
			Version     string `json:"version"`
			AppVersion  string `json:"appVersion"`
			KubeVersion string `json:"kubeVersion"`
		} `json:"metadata"`
	} `json:"chart"`
	Manifest string `json:"manifest"` // the rendered objects Helm applied
}

// helmDriver is one Helm storage driver: where its revisions live and how
// to read one revision's payload.
type helmDriver struct {
	name    string // HELM_DRIVER value: "secrets" or "configmaps"
	gvr     schema.GroupVersionResource
	payload func(ctx context.Context, namespace, name string) ([]byte, error)
}

// helmDrivers are the storage drivers collectHelm reads. Helm's sql driver
// keeps releases in an external database the cluster cannot show, and
// GitOps tools that render charts with helm template (Argo CD) create no
// release object at all; neither is assessed here. Add-on image matching
// still sees what they deploy.
func helmDrivers(kube kubernetes.Interface) []helmDriver {
	return []helmDriver{
		{
			name: "secrets",
			gvr:  corev1.SchemeGroupVersion.WithResource("secrets"),
			payload: func(ctx context.Context, ns, name string) ([]byte, error) {
				s, err := kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				if s.Type != helmSecretType {
					return nil, fmt.Errorf("secret %s/%s is of type %q, not %s", ns, name, s.Type, helmSecretType)
				}
				return s.Data["release"], nil
			},
		},
		{
			name: "configmaps",
			gvr:  corev1.SchemeGroupVersion.WithResource("configmaps"),
			payload: func(ctx context.Context, ns, name string) ([]byte, error) {
				cm, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return []byte(cm.Data["release"]), nil
			},
		},
	}
}

// helmRevision is one stored revision, known from its object's labels.
type helmRevision struct {
	driver   int    // index into the drivers slice
	object   string // Secret or ConfigMap name
	revision int
	status   string
}

// collectHelm reads Helm v3 releases from the secrets and configmaps
// storage drivers and records, per (namespace, release name), the
// installed revision (see installedRevision): its chart, versions, chart
// kubeVersion, and the objects in its stored manifest at APIs the KB
// lifecycle data flags.
//
// Memory stays bounded by one release however long the history: each
// driver's objects are listed metadata-only (label owner=helm, paged),
// the labels Helm writes (name, version, status) pick the revision, and
// only that one object is fetched and decoded. Each release payload is a
// gzipped JSON document that holds the whole chart, so decoding every
// revision OOM-killed the agent on clusters with long histories (#24).
//
// Drivers degrade independently for reads: a driver whose list fails is
// skipped and named in the reason, and the releases the others hold are
// still recorded. The capability is unavailable — a gap the report shows —
// when the secrets driver (Helm's default, so where releases almost always
// are) failed, or when a driver failed and nothing was read: a role such as
// the built-in view ClusterRole reads ConfigMaps but not Secrets, and "0
// via configmaps" from it says nothing about the cluster. Only a failed
// configmaps driver beside readable Secret releases leaves the capability
// available (an install from a chart that predates the configmaps rule).
// The reason counts the releases per driver ("helm releases: 3 via
// secrets, 0 via configmaps") so the inventory shows where release data
// came from (runSteps only carries a reason for an available capability
// on a partialError, so one is returned whenever the capability is
// available). A release whose object cannot be fetched is skipped and
// counted in the reason, and when none can be (list but no get), the
// capability is unavailable; a release whose payload cannot be decoded is
// skipped and counted too — one corrupt release must not fail the
// capability. Any of these failures leaves an available capability
// Partial, naming the drivers and releases it skipped.
func collectHelm(ctx context.Context, kube kubernetes.Interface, meta metadata.Interface, lifecycle []kb.APILifecycleEntry, inv *inventory.Inventory) error {
	type releaseKey struct{ namespace, name string }
	drivers := helmDrivers(kube)
	revisions := map[releaseKey][]helmRevision{}
	listErrs := make([]error, len(drivers))
	var failed []string  // "<what> not read: <err>", drivers first
	var skipped []string // drivers, then releases ("namespace/name"), not read
	for d, drv := range drivers {
		err := listMetadata(ctx, meta, drv.gvr, metav1.ListOptions{LabelSelector: "owner=helm"}, func(m metav1.PartialObjectMetadata) {
			name, rev, ok := helmRevisionOf(m)
			if !ok {
				return
			}
			k := releaseKey{m.Namespace, name}
			revisions[k] = append(revisions[k], helmRevision{driver: d, object: m.Name, revision: rev, status: m.Labels["status"]})
		})
		if err != nil {
			listErrs[d] = err
			failed = append(failed, fmt.Sprintf("%s not read: %v", drv.name, err))
			skipped = append(skipped, drv.name)
		}
	}
	if len(failed) == len(drivers) {
		return errors.New(strings.Join(failed, "; "))
	}

	flagged := map[gvk]bool{}
	for _, e := range lifecycle {
		if e.Deprecated != nil || e.Removed != nil {
			flagged[gvk{e.Group, e.Version, e.Kind}] = true
		}
	}
	keys := slices.SortedFunc(maps.Keys(revisions), func(a, b releaseKey) int {
		return cmp.Or(cmp.Compare(a.namespace, b.namespace), cmp.Compare(a.name, b.name))
	})
	perDriver := make([]int, len(drivers))
	var rels []inventory.HelmRelease
	unread, firstUnread := 0, ""
	undecodable, firstUndecodable := 0, ""
	var skippedReleases []string
	for _, k := range keys {
		// One driver's history per release: revision numbers of two
		// histories (HELM_DRIVER changed) are not comparable, so the first
		// driver holding the release — secrets, Helm's default — wins.
		revs := revisions[k]
		first := slices.MinFunc(revs, func(a, b helmRevision) int { return a.driver - b.driver }).driver
		r, ok := installedRevision(slices.DeleteFunc(revs, func(r helmRevision) bool { return r.driver != first }))
		if !ok {
			continue
		}
		data, err := drivers[r.driver].payload(ctx, k.namespace, r.object)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue // deleted since the list: Helm pruned history
			}
			if unread++; unread == 1 {
				firstUnread = fmt.Sprintf("%s/%s: %v", k.namespace, k.name, err)
			}
			skippedReleases = append(skippedReleases, k.namespace+"/"+k.name)
			continue
		}
		doc, err := decodeHelmRelease(data)
		if err != nil { // one corrupt release must not fail the capability
			if undecodable++; undecodable == 1 {
				firstUndecodable = fmt.Sprintf("%s/%s: %v", k.namespace, k.name, err)
			}
			skippedReleases = append(skippedReleases, k.namespace+"/"+k.name)
			continue
		}
		status := r.status
		if status == "" {
			status = doc.Info.Status
		}
		perDriver[r.driver]++
		rels = append(rels, inventory.HelmRelease{
			Name:         k.name,
			Namespace:    k.namespace,
			ChartName:    doc.Chart.Metadata.Name,
			ChartVersion: doc.Chart.Metadata.Version,
			AppVersion:   doc.Chart.Metadata.AppVersion,
			KubeVersion:  doc.Chart.Metadata.KubeVersion,
			Status:       status,
			Revision:     r.revision,
			ManifestAPIs: manifestAPIs(doc.Manifest, flagged),
		})
	}
	if unread > 0 {
		failed = append(failed, fmt.Sprintf("%d release(s) not read, first %s", unread, firstUnread))
		if len(rels) == 0 { // listed but none readable (e.g. list without get)
			return errors.New(strings.Join(failed, "; "))
		}
	}
	if undecodable > 0 {
		failed = append(failed, fmt.Sprintf("%d release(s) not decodable, first %s", undecodable, firstUndecodable))
	}
	skipped = append(skipped, skippedReleases...) // keys are sorted
	if len(rels) > 0 {
		inv.HelmReleases = rels
	}

	var counts []string
	for d, drv := range drivers {
		if listErrs[d] == nil {
			counts = append(counts, fmt.Sprintf("%d via %s", perDriver[d], drv.name))
		}
	}
	msg := strings.Join(append([]string{"helm releases: " + strings.Join(counts, ", ")}, failed...), "; ")
	if listErrs[0] != nil || (len(rels) == 0 && len(failed) > 0) {
		return errors.New(msg) // secrets unread, or nothing read and a driver failed: not assessed
	}
	return partialError{msg: msg, incomplete: len(failed) > 0, skipped: skipped}
}

// listMetadata lists one resource cluster-wide, metadata-only and paged,
// calling fn for every item.
func listMetadata(ctx context.Context, meta metadata.Interface, gvr schema.GroupVersionResource, opts metav1.ListOptions, fn func(metav1.PartialObjectMetadata)) error {
	opts.Limit = listPageSize
	for {
		l, err := meta.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list %s: %w", gvr.Resource, err)
		}
		for _, m := range l.Items {
			fn(m)
		}
		if l.Continue == "" {
			return nil
		}
		opts.Continue = l.Continue
	}
}

// helmRevisionOf reads a stored revision's release name and number from
// the labels Helm writes, falling back to the object name
// "sh.helm.release.v1.<name>.v<N>"; ok is false for objects that are not
// Helm v3 release storage.
func helmRevisionOf(m metav1.PartialObjectMetadata) (name string, rev int, ok bool) {
	rest, ok := strings.CutPrefix(m.Name, helmObjectPrefix)
	if !ok {
		return "", 0, false
	}
	name = m.Labels["name"]
	if name == "" {
		if i := strings.LastIndex(rest, ".v"); i > 0 {
			name = rest[:i]
		}
	}
	rev, err := strconv.Atoi(m.Labels["version"])
	if err != nil {
		rev = releaseRevision(m.Name)
	}
	return name, rev, name != ""
}

// installedRevision picks the revision whose chart is what runs, by the
// newest revision's status:
//
//   - uninstalled, uninstalling: nothing (helm uninstall --keep-history
//     keeps the history but deletes the resources);
//   - failed: the newest deployed or superseded revision before it, whose
//     resources a failed upgrade leaves running; nothing when there is
//     none (a failed install);
//   - anything else (deployed, pending-install/upgrade/rollback, or a
//     status this code does not know): the newest revision, which is being
//     or has been applied.
func installedRevision(revs []helmRevision) (helmRevision, bool) {
	slices.SortFunc(revs, func(a, b helmRevision) int { return b.revision - a.revision })
	switch revs[0].status {
	case "uninstalled", "uninstalling":
		return helmRevision{}, false
	case "failed":
		for _, r := range revs[1:] {
			if r.status == "deployed" || r.status == "superseded" {
				return r, true
			}
		}
		return helmRevision{}, false
	}
	return revs[0], true
}

// manifestAPIs parses a release's stored manifest with the --files parser
// and returns, per GVK, the objects at a flagged group/version/kind.
// Documents that do not parse are skipped: the manifest is what Helm
// applied, and a partial result beats none.
func manifestAPIs(manifest string, flagged map[gvk]bool) []inventory.APIUsage {
	if len(flagged) == 0 || manifest == "" {
		return nil
	}
	objs, _, _, _ := parseManifestStream(strings.NewReader(manifest)) // a strings.Reader never fails
	objs = slices.DeleteFunc(objs, func(o manifestObject) bool { return !flagged[gvk{o.group, o.version, o.kind}] })
	counts := map[gvk]*inventory.APIUsage{}
	accumulate(counts, objs)
	return usageRows(counts)
}

// releaseRevision parses N from "sh.helm.release.v1.<name>.v<N>"; 0 if absent.
func releaseRevision(secretName string) int {
	i := strings.LastIndex(secretName, ".v")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(secretName[i+2:])
	if err != nil {
		return 0
	}
	return n
}

// decodeHelmRelease decodes a stored release payload: the Secret's
// Data["release"] (client-go has already removed the Secret's own base64)
// or the ConfigMap's Data["release"]. Either is a base64 string wrapping
// gzip(JSON). Order: base64 → gzip magic check → gunzip → JSON.
func decodeHelmRelease(data []byte) (helmReleaseDoc, error) {
	var doc helmReleaseDoc
	raw, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return doc, fmt.Errorf("base64: %w", err)
	}
	if len(raw) < 3 || raw[0] != 0x1f || raw[1] != 0x8b || raw[2] != 0x08 {
		return doc, fmt.Errorf("release payload is not gzip")
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return doc, fmt.Errorf("gunzip: %w", err)
	}
	defer zr.Close()
	jsonBytes, err := io.ReadAll(zr)
	if err != nil {
		return doc, fmt.Errorf("gunzip read: %w", err)
	}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return doc, fmt.Errorf("release json: %w", err)
	}
	return doc, nil
}
