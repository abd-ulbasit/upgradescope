package collect

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
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
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

	// maxStoredReleaseBytes bounds a stored payload before it is base64
	// decoded. Kubernetes caps a Secret's or ConfigMap's data at 1 MiB, so
	// only a misbehaving apiserver serves more.
	maxStoredReleaseBytes = 4 << 20
	// maxReleaseJSONBytes bounds one release's decompressed JSON (#168): a
	// gzip of 700 MiB of whitespace fits a 951 KB Secret, and decompressing
	// it whole took a scan to 1.93 GB and OOM-killed the agent. Real releases
	// are a few MiB (measured with helm install --dry-run=client -o json:
	// kube-prometheus-stack 91.9.0 2.6 MiB, base 1.30.5 2.0 MiB,
	// cert-manager v1.21.2 1.7 MiB, istiod 1.30.5 0.4 MiB, each gzipping
	// about 7–9×), and at that ratio the 768 KiB of gzip a 1 MiB Secret can
	// hold decodes to under 7 MiB. The largest chart found, kyverno 3.9.1
	// (5.6 MiB of CRDs rendered from templates), comes to about 14 MiB of
	// JSON but 2 MiB of gzip, more than a Secret holds. 16 MiB leaves over
	// 2× headroom; a release past it is not decodable, a gap the report
	// shows. The cap is also what a release's decoding costs: the JSON
	// read whole plus the manifest decoded from it.
	maxReleaseJSONBytes = 16 << 20
	// manifestChunkBytes bounds how much of a release's manifest is parsed
	// at once (see manifestAPIs): parsing amplifies its input, up to about
	// 26× for a manifest of tiny objects.
	manifestChunkBytes = 1 << 20
	// maxManifestDocBytes bounds one document of a release's manifest,
	// which is parsed whole: about 11× its size in heap for a document of
	// newlines or comments. The largest real one found is 1.4 MiB (kyverno's
	// policies.kyverno.io CRD; kube-prometheus-stack's largest, the
	// prometheuses CRD, is 0.8 MiB), and etcd by default refuses a write
	// over 1.5 MiB. A larger document is not parsed.
	maxManifestDocBytes = 2 << 20
	// maxManifestNodes bounds the YAML nodes (see yamlNodeBound) parsed at
	// once, in one run of documents or one document: each costs about 550
	// bytes of heap, so 64Ki nodes is about 35 MiB. Measured on the
	// manifests Helm stores (helm template, no crds/ directory), the
	// largest real document bounds at 48,550: argo-cd 10.9.6's
	// applicationsets.argoproj.io CRD, rendered from templates/crds. The
	// densest is external-secrets 2.11.0's clustersecretstores CRD, 43,220
	// in 688 KiB; then kyverno 3.9.1's policies CRD 44,746,
	// kube-prometheus-stack's prometheuses CRD 31,078,
	// opentelemetry-operator's collectors CRD 28,146, and keda, cnpg,
	// strimzi and istio base under 22,000. crossplane 2.4.2 and
	// tigera-operator store no CRDs in the release. Doubling the bound
	// would double what a planted run costs; a document over it is not
	// parsed, a gap the report shows.
	maxManifestNodes = 1 << 16
)

// errReleaseTooLarge marks a release payload over maxStoredReleaseBytes
// stored or maxReleaseJSONBytes decompressed: not decodable, never read
// whole.
var errReleaseTooLarge = errors.New("release payload too large")

// errManifestDocTooLarge marks a release manifest with a document over
// maxManifestDocBytes or maxManifestNodes, which is not parsed.
var errManifestDocTooLarge = errors.New("manifest document too large")

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
// release object at all; neither is read here. collectHelmStep reads the
// charts those tools declare, and add-on image matching still sees what
// they deploy.
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
	uid      types.UID // with resourceVersion, identifies what the object held when listed
	rv       string
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
// One release is bounded too (#168), against anyone who can create a
// Secret or ConfigMap in one namespace and so plant a release:
// decodeHelmRelease stops decompressing past maxReleaseJSONBytes, so a gzip
// bomb is a release not decodable (payload too large), and manifestAPIs
// parses the manifest in runs bounded in bytes and YAML nodes, so a
// manifest built to amplify is parsed in bounded memory, and a document too
// large for one run is not parsed. Either is a gap naming the release, not
// an OOM-killed agent.
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
// capability — and a release whose manifest is not fully parsed is
// recorded with what the rest holds, and counted. A release whose history
// holds failed revisions only has no revision to judge (installedRevision):
// it is not fetched, and is counted and named too, whether or not anything
// else was read (#239). Any of these failures leaves an available
// capability Partial, naming the drivers and releases it skipped.
func collectHelm(ctx context.Context, kube kubernetes.Interface, meta metadata.Interface, lifecycle []kb.APILifecycleEntry, inv *inventory.Inventory) error {
	return collectHelmWith(ctx, kube, meta, lifecycle, nil, nil, inv)
}

// collectHelmWith is collectHelm with a cache of what earlier calls decoded
// (nil reads every release, as a one-shot scan does), reading Helm's
// storage only in namespaces when that is not empty (nil reads the whole
// cluster, as a one-shot scan does; see collectHelmFetching).
func collectHelmWith(ctx context.Context, kube kubernetes.Interface, meta metadata.Interface, lifecycle []kb.APILifecycleEntry, cache *HelmCache, namespaces []string, inv *inventory.Inventory) error {
	return collectHelmFetching(ctx, kube, meta, lifecycle, cache, namespaces, helmFetchWorkers, inv)
}

// helmSkippedOtherNamespaces is the helm capability's Skipped entry when
// Helm's storage was read in some namespaces only (#344: the agent's
// --helm-namespaces, the chart's rbac.helmSecretsNamespaces). It has no
// slash, so the engine reads it as it reads a storage driver not read:
// every release unassessed, since a release's finding does not say whether
// its namespace was listed, and one in another namespace was never read.
const helmSkippedOtherNamespaces = "releases outside the listed namespaces"

// collectHelmFetching is collectHelmWith fetching the releases the cache
// does not hold on up to workers goroutines (see startFetching); 1 fetches
// them one at a time, as before #226. Whatever workers is, the releases are
// decoded one at a time, in the order of their keys, on the caller's
// goroutine, so the inventory, the reasons and the cache come out the same.
//
// With namespaces (#344), each driver is listed in each of them in turn,
// sorted and each once, and never across the cluster: the agent then holds
// a Role in each and no cluster-wide grant. A driver whose list fails in
// any of them is a driver not read, as one whose cluster-wide list fails
// is, and the other namespaces are still listed. Whatever was read, the
// capability is partial, saying which namespaces were read and naming
// helmSkippedOtherNamespaces: releases elsewhere were not assessed.
func collectHelmFetching(ctx context.Context, kube kubernetes.Interface, meta metadata.Interface, lifecycle []kb.APILifecycleEntry, cache *HelmCache, namespaces []string, workers int, inv *inventory.Inventory) error {
	type releaseKey struct{ namespace, name string }
	drivers := helmDrivers(kube)
	revisions := map[releaseKey][]helmRevision{}
	listErrs := make([]error, len(drivers))
	var failed []string  // "<what> not read: <err>", drivers first
	var skipped []string // drivers, then releases ("namespace/name"), not read
	scope := []string{metav1.NamespaceAll}
	var scoped string // the note of a read in some namespaces only
	if len(namespaces) > 0 {
		scope = slices.Compact(slices.Sorted(slices.Values(namespaces)))
		scoped = fmt.Sprintf("Helm releases read only in namespaces %s (rbac.helmSecretsNamespaces); releases in other namespaces were not assessed", strings.Join(scope, ", "))
	}
	for d, drv := range drivers {
		more := 0 // namespaces after the first whose list failed too
		for _, ns := range scope {
			err := listMetadata(ctx, meta, drv.gvr, ns, metav1.ListOptions{LabelSelector: "owner=helm"}, func(m metav1.PartialObjectMetadata) {
				name, rev, ok := helmRevisionOf(m)
				if !ok {
					return
				}
				k := releaseKey{m.Namespace, name}
				revisions[k] = append(revisions[k], helmRevision{driver: d, object: m.Name, revision: rev, status: m.Labels["status"], uid: m.UID, rv: m.ResourceVersion})
			})
			switch {
			case err == nil:
			case listErrs[d] == nil:
				listErrs[d] = err
			default:
				more++
			}
		}
		if err := listErrs[d]; err != nil {
			msg := fmt.Sprintf("%s not read: %v", drv.name, err)
			if more > 0 {
				msg += fmt.Sprintf(" (and in %d more namespace(s))", more)
			}
			failed = append(failed, msg)
			skipped = append(skipped, drv.name)
		}
	}
	if len(failed) == len(drivers) {
		if scoped != "" {
			failed = append(failed, scoped)
		}
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
	cache.begin(flaggedFingerprint(flagged))
	seen := map[helmCacheKey]bool{}
	perDriver := make([]int, len(drivers))
	var rels []inventory.HelmRelease
	unread, firstUnread := 0, ""
	undecodable, firstUndecodable := 0, ""
	unparsed, firstUnparsed := 0, ""
	unjudged, firstUnjudged := 0, ""
	var skippedReleases []string
	// The installed revision of each release, in key order, and whether
	// the cache holds it: the cache is read here, before any fetch, and
	// written only below, on this goroutine.
	type installed struct {
		key   releaseKey
		rev   helmRevision
		ck    helmCacheKey
		entry helmCacheEntry
		hit   bool
	}
	var todo []installed
	var misses []int // indexes into todo of the releases to fetch, in key order
	for _, k := range keys {
		// One driver's history per release: revision numbers of two
		// histories (HELM_DRIVER changed) are not comparable, so the first
		// driver holding the release — secrets, Helm's default — wins.
		revs := revisions[k]
		first := slices.MinFunc(revs, func(a, b helmRevision) int { return a.driver - b.driver }).driver
		r, pick := installedRevision(slices.DeleteFunc(revs, func(r helmRevision) bool { return r.driver != first }))
		switch pick {
		case revisionGone:
			continue
		case revisionUnjudged: // never fetched: no revision of it is judged
			if unjudged++; unjudged == 1 {
				firstUnjudged = k.namespace + "/" + k.name
			}
			skippedReleases = append(skippedReleases, k.namespace+"/"+k.name)
			continue
		}
		ck := helmCacheKey{driver: r.driver, namespace: k.namespace, object: r.object, uid: r.uid, rv: r.rv}
		entry, hit := cache.get(ck)
		if !hit {
			misses = append(misses, len(todo))
		}
		todo = append(todo, installed{key: k, rev: r, ck: ck, entry: entry, hit: hit})
	}
	// The payloads come back in the order of misses, so the releases are
	// visited exactly as when they were fetched one at a time.
	fetcher := startFetching(ctx, len(misses), workers, func(ctx context.Context, m int) ([]byte, error) {
		t := todo[misses[m]]
		return drivers[t.rev.driver].payload(ctx, t.key.namespace, t.rev.object)
	})
	defer fetcher.stop()
	for _, t := range todo {
		k, r, ck, entry := t.key, t.rev, t.ck, t.entry
		seen[ck] = true
		if !t.hit {
			data, err := fetcher.next()
			if err != nil {
				if apierrors.IsNotFound(err) {
					continue // deleted since the list: Helm pruned history
				}
				if unread++; unread == 1 {
					firstUnread = fmt.Sprintf("%s/%s: %v", k.namespace, k.name, err)
				}
				skippedReleases = append(skippedReleases, k.namespace+"/"+k.name)
				delete(seen, ck) // nothing known about it: keep nothing
				continue
			}
			entry = decodeHelmEntry(data, flagged)
			cache.put(ck, entry)
		}
		if entry.decodeErr != "" { // one corrupt release must not fail the capability
			if undecodable++; undecodable == 1 {
				firstUndecodable = fmt.Sprintf("%s/%s: %s", k.namespace, k.name, entry.decodeErr)
			}
			skippedReleases = append(skippedReleases, k.namespace+"/"+k.name)
			continue
		}
		status := r.status
		if status == "" {
			status = entry.status
		}
		if entry.parseErr != "" { // recorded with what the rest of its manifest holds
			if unparsed++; unparsed == 1 {
				firstUnparsed = fmt.Sprintf("%s/%s: %s", k.namespace, k.name, entry.parseErr)
			}
			skippedReleases = append(skippedReleases, k.namespace+"/"+k.name)
		}
		perDriver[r.driver]++
		rels = append(rels, inventory.HelmRelease{
			Name:         k.name,
			Namespace:    k.namespace,
			ChartName:    entry.chartName,
			ChartVersion: entry.chartVersion,
			AppVersion:   entry.appVersion,
			KubeVersion:  entry.kubeVersion,
			Status:       status,
			Revision:     r.revision,
			ManifestAPIs: cloneAPIUsage(entry.apis),
		})
	}
	cache.prune(seen, func(driver int) bool { return listErrs[driver] == nil })
	if unread > 0 {
		failed = append(failed, fmt.Sprintf("%d release(s) not read, first %s", unread, firstUnread))
	}
	if undecodable > 0 {
		failed = append(failed, fmt.Sprintf("%d release(s) not decodable, first %s", undecodable, firstUndecodable))
	}
	if unparsed > 0 {
		failed = append(failed, fmt.Sprintf("%d release manifest(s) not fully parsed, first %s", unparsed, firstUnparsed))
	}
	// Unavailable only for what was not read: the default driver, or every
	// release when something failed (a driver, or each listed release's GET,
	// as with list without get). A release with no revision to judge was
	// read, and leaves the capability partial however many there are; its
	// count is in the reason either way.
	notAssessed := listErrs[0] != nil || (len(rels) == 0 && len(failed) > 0)
	if unjudged > 0 {
		failed = append(failed, fmt.Sprintf("%d release(s) with only failed revisions not assessed (no deployed or superseded revision to judge their chart and stored manifest by), first %s", unjudged, firstUnjudged))
	}
	skipped = append(skipped, skippedReleases...) // keys are sorted
	// After the counts and failures, which decide availability above.
	if scoped != "" {
		failed = append(failed, scoped)
		skipped = append(skipped, helmSkippedOtherNamespaces)
	}
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
	if notAssessed {
		return errors.New(msg) // secrets unread, or nothing read and something failed: not assessed
	}
	return partialError{msg: msg, incomplete: len(failed) > 0, skipped: skipped}
}

// listMetadata lists one resource in namespace (metav1.NamespaceAll:
// cluster-wide), metadata-only and paged, calling fn for every item.
func listMetadata(ctx context.Context, meta metadata.Interface, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions, fn func(metav1.PartialObjectMetadata)) error {
	opts.Limit = listPageSize
	for {
		l, err := meta.Resource(gvr).Namespace(namespace).List(ctx, opts)
		if err != nil {
			if namespace != metav1.NamespaceAll {
				return fmt.Errorf("list %s in namespace %s: %w", gvr.Resource, namespace, err)
			}
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
	// The name label is a free label value, written by whoever can label
	// the object; Helm's release names are RFC 1123 subdomains. One that is
	// not is not the release's (the apiserver validated the object's name,
	// which carries it too), so the name comes from there.
	name = m.Labels["name"]
	if name != "" && len(content.IsDNS1123Subdomain(name)) > 0 {
		name = ""
	}
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

// revisionPick is what installedRevision found in a release's history.
type revisionPick int

const (
	revisionInstalled revisionPick = iota // a revision to judge the release by
	revisionGone                          // uninstalled: nothing runs, nothing to report
	revisionUnjudged                      // failed revisions only: something may run, nothing to judge it by
)

// installedRevision picks the revision whose chart is what runs, by the
// newest revision's status:
//
//   - uninstalled, uninstalling: nothing (helm uninstall --keep-history
//     keeps the history but deletes the resources), revisionGone;
//   - failed: the newest deployed revision, as Helm's Releases.Deployed
//     finds it, else the newest superseded one: what a failed upgrade
//     leaves running, and the manifest a helm upgrade builds from when
//     there is a deployed one (#239). With neither, the history is failed
//     revisions only (a failed install, such as helm install --wait timing
//     out with every resource running): revisionUnjudged, which the caller
//     reports as a gap naming the release, since a failed revision's
//     manifest says what Helm tried to apply, not what runs;
//   - anything else (deployed, pending-install/upgrade/rollback, or a
//     status this code does not know): the newest revision, which is being
//     or has been applied.
func installedRevision(revs []helmRevision) (helmRevision, revisionPick) {
	slices.SortFunc(revs, func(a, b helmRevision) int { return b.revision - a.revision })
	switch revs[0].status {
	case "uninstalled", "uninstalling":
		return helmRevision{}, revisionGone
	case "failed":
		for _, status := range []string{"deployed", "superseded"} {
			if i := slices.IndexFunc(revs[1:], func(r helmRevision) bool { return r.status == status }); i >= 0 {
				return revs[1+i], revisionInstalled
			}
		}
		return helmRevision{}, revisionUnjudged
	}
	return revs[0], revisionInstalled
}

// manifestAPIs parses a release's stored manifest with the --files parser
// and returns, per GVK, the objects at a flagged group/version/kind.
// Documents that do not parse are skipped: the manifest is what Helm
// applied, and a partial result beats none.
//
// The manifest is parsed a run of documents at a time (see
// splitManifest), each run's flagged objects counted before the next is
// read, because parsing amplifies its input (#168): the parser keeps every
// object of a stream before the flagged ones are picked, and builds about
// 550 bytes of heap per YAML node: parsed whole, a 32 MiB manifest of tiny
// ConfigMaps took 564 MiB, and one 1 MiB document of "- -" lines 240 MiB.
// A document over maxManifestDocBytes or maxManifestNodes is not parsed:
// err (errManifestDocTooLarge) names the first, and the objects of the
// other documents are still returned.
func manifestAPIs(manifest string, flagged map[gvk]bool) (rows []inventory.APIUsage, err error) {
	if len(flagged) == 0 || manifest == "" {
		return nil, nil
	}
	counts := map[gvk]*inventory.APIUsage{}
	tooLarge, firstLine := 0, 0
	splitManifest(manifest, func(text string, line int, whole bool) {
		if strings.TrimSpace(text) == "" {
			return // blank: holds nothing
		}
		if !whole {
			if tooLarge++; tooLarge == 1 {
				firstLine = line
			}
			return
		}
		objs, _, _, _ := parseManifestStream(strings.NewReader(text)) // a strings.Reader never fails
		objs = slices.DeleteFunc(objs, func(o manifestObject) bool { return !flagged[gvk{o.group, o.version, o.kind}] })
		for i := range objs {
			objs[i].ref.Line += line - 1 // lines of the run → lines of the manifest
		}
		accumulate(counts, objs)
	})
	if tooLarge > 0 {
		err = fmt.Errorf("%w: %d document(s) over %d MiB or %d YAML nodes not parsed, first at manifest line %d",
			errManifestDocTooLarge, tooLarge, maxManifestDocBytes>>20, maxManifestNodes, firstLine)
	}
	return usageRows(counts), err
}

// splitManifest cuts a release manifest at its document separators (lines
// starting with "---", where the --files parser splits it too) into runs
// of whole documents of at most manifestChunkBytes and maxManifestNodes
// YAML nodes, or one larger document, and calls fn with each run and the
// manifest line it starts on. A document over maxManifestDocBytes or
// maxManifestNodes is passed alone with whole false, to be skipped rather
// than parsed. Splitting stops at an invalid separator ("---" followed by
// more than a comment), where the parser stops reading the stream too. A
// manifest that opens as JSON, which Helm never writes, is not split (the
// parser reads JSON values across lines): it is one document.
func splitManifest(manifest string, fn func(text string, line int, whole bool)) {
	if utilyaml.IsJSONBuffer([]byte(manifest[:min(len(manifest), jsonPeek)])) {
		fn(manifest, 1, len(manifest) <= maxManifestDocBytes && yamlNodeBound(manifest) <= maxManifestNodes)
		return
	}
	// span is a stretch of the manifest from offset start, which begins
	// line line, bounding nodes YAML nodes (see yamlNodeBound).
	type span struct{ start, line, nodes int }
	run := span{0, 1, 0} // whole documents not yet passed to fn
	doc := span{0, 1, 2} // the document being read: its root and document nodes
	// endDoc ends the document at offset end, which begins line endLine.
	endDoc := func(end, endLine int) {
		if doc.start > run.start && (end-run.start > manifestChunkBytes || run.nodes+doc.nodes > maxManifestNodes) {
			fn(manifest[run.start:doc.start], run.line, true) // the run is full without it
			run = span{doc.start, doc.line, 0}
		}
		if end-doc.start > maxManifestDocBytes || doc.nodes > maxManifestNodes {
			fn(manifest[doc.start:end], doc.line, false)
			run = span{end, endLine, 0}
		} else {
			run.nodes += doc.nodes
		}
		doc = span{end, endLine, 2}
	}
	line, invalid := 1, false
	for off := 0; off < len(manifest) && !invalid; line++ {
		end := len(manifest)
		if i := strings.IndexByte(manifest[off:], '\n'); i >= 0 {
			end = off + i + 1
		}
		if rest, ok := strings.CutPrefix(manifest[off:end], "---"); ok {
			endDoc(off, line)
			t := strings.TrimSpace(rest)
			invalid = t != "" && t[0] != '#' // the parser reads nothing past it
		} else {
			doc.nodes += yamlNodeBound(manifest[off:end])
		}
		off = end
	}
	if !invalid {
		endDoc(len(manifest), line)
	}
	if run.start < doc.start {
		fn(manifest[run.start:doc.start], run.line, true)
	}
}

// yamlNodeBound bounds the YAML nodes text adds to a document. Every node
// but a document's root is introduced by one of : - , [ { ? (a mapping
// entry, a sequence item, a flow collection's entries), and none
// introduces more than two (a mapping entry's key and value), so twice
// their count is a bound: loose for real manifests, whose descriptions are
// full of these characters, and tight for a planted one ("- -" lines).
// Parsing costs about 500 bytes of heap per node, whatever the bytes
// holding it.
func yamlNodeBound(text string) int {
	n := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ':', '-', ',', '[', '{', '?':
			n += 2
		}
	}
	return n
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
// gzip(JSON). Order: size check → base64 → gzip magic check → gunzip,
// stopped past maxReleaseJSONBytes → JSON. Every layer is bounded: the
// stored payload by maxStoredReleaseBytes, its base64 decoding (which only
// shrinks it) by that, and the gzip, the one layer that expands, by
// maxReleaseJSONBytes. An over-cap payload is errReleaseTooLarge, which
// collectHelm counts as not decodable.
//
// The JSON is read whole into one buffer sized by the gzip trailer's
// decompressed size, then unmarshalled: a json.Decoder buffers a value
// whole too, doubling its buffer to get there, which took decoding a 32
// MiB release to over 100 MiB of heap. The buffer never grows, so the
// trailer is trusted only as far as it can be checked. Helm writes one
// gzip member, and only one member's trailer is the payload's last 4
// bytes, so the reader stops after the first member and anything after it
// is an error. Decompression stops past what the trailer says, so a
// trailer that understates the size (forged, or an empty second member's)
// is an error before it costs more than the buffer. One that overstates it
// costs a buffer of at most maxReleaseJSONBytes, and the gzip reader then
// fails it with the CRC at the end of the stream, which is always read.
func decodeHelmRelease(data []byte) (helmReleaseDoc, error) {
	var doc helmReleaseDoc
	if len(data) > maxStoredReleaseBytes {
		return doc, fmt.Errorf("%w: over %d MiB stored", errReleaseTooLarge, maxStoredReleaseBytes>>20)
	}
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	n, err := base64.StdEncoding.Decode(raw, data)
	if err != nil {
		return doc, fmt.Errorf("base64: %w", err)
	}
	raw = raw[:n]
	if len(raw) < 3 || raw[0] != 0x1f || raw[1] != 0x8b || raw[2] != 0x08 {
		return doc, fmt.Errorf("release payload is not gzip")
	}
	br := bytes.NewReader(raw)
	zr, err := gzip.NewReader(br)
	if err != nil {
		return doc, fmt.Errorf("gunzip: %w", err)
	}
	defer zr.Close()
	zr.Multistream(false)
	size := min(int(binary.LittleEndian.Uint32(raw[len(raw)-4:])), maxReleaseJSONBytes) // ISIZE: gzip's last 4 bytes
	buf := bytes.NewBuffer(make([]byte, 0, size+bytes.MinRead))
	if _, err := buf.ReadFrom(&boundedReader{r: zr, left: int64(size)}); err != nil {
		switch {
		case errors.Is(err, errReleaseTooLarge) && size == maxReleaseJSONBytes:
			return doc, fmt.Errorf("%w: over %d MiB decompressed", errReleaseTooLarge, maxReleaseJSONBytes>>20)
		case errors.Is(err, errReleaseTooLarge):
			return doc, errors.New("gunzip: decompresses past its size trailer")
		}
		return doc, fmt.Errorf("gunzip read: %w", err)
	}
	if br.Len() > 0 { // gzip reads its flate.Reader byte by byte, never past the member
		return doc, errors.New("gunzip: data after the gzip stream")
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		return doc, fmt.Errorf("release json: %w", err)
	}
	return doc, nil
}

// boundedReader reads at most left more bytes from r and fails with
// errReleaseTooLarge, rather than ending as io.LimitReader does, when r
// holds more: a truncated document must not pass for a whole one. It reads
// one byte past the cap to tell the two apart, and keeps r's first error
// other than io.EOF in err.
type boundedReader struct {
	r    io.Reader
	left int64
	err  error
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.r.Read(p)
	if int64(n) > b.left {
		b.err = errReleaseTooLarge
		return 0, b.err
	}
	b.left -= int64(n)
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}
