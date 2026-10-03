package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/sarif"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// helmRev is one stored Helm release revision.
type helmRev struct {
	ns, release  string
	rev          int
	status       string
	chart        string
	chartVersion string
	appVersion   string
	kubeVersion  string
	manifest     string
	uid, rv      string // the object's UID and resourceVersion; empty unless a test sets them
}

// payload encodes the revision exactly as Helm v3 stores it:
// base64(gzip(JSON)). client-go strips a Secret's outer base64, leaving
// this inner base64 string; a ConfigMap stores it as is.
func (r helmRev) payload(t *testing.T) []byte {
	t.Helper()
	doc, err := json.Marshal(map[string]any{
		"name": r.release, "version": r.rev, "namespace": r.ns,
		"info": map[string]any{"status": r.status},
		"chart": map[string]any{"metadata": map[string]any{
			"name": r.chart, "version": r.chartVersion, "appVersion": r.appVersion, "kubeVersion": r.kubeVersion,
		}},
		"manifest": r.manifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(doc); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))
}

// objectMeta is the storage object's metadata as Helm's secrets and
// configmaps drivers write it.
func (r helmRev) objectMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:            fmt.Sprintf("sh.helm.release.v1.%s.v%d", r.release, r.rev),
		Namespace:       r.ns,
		UID:             types.UID(r.uid),
		ResourceVersion: r.rv,
		Labels: map[string]string{
			"owner": "helm", "name": r.release, "status": r.status, "version": fmt.Sprint(r.rev),
			"modifiedAt": "1700000000",
		},
	}
}

func helmSecret(t *testing.T, r helmRev) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: r.objectMeta(),
		Type:       "helm.sh/release.v1",
		Data:       map[string][]byte{"release": r.payload(t)},
	}
}

func helmConfigMap(t *testing.T, r helmRev) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: r.objectMeta(),
		Data:       map[string]string{"release": string(r.payload(t))},
	}
}

// helmClients serves objs from a typed fake (GETs) and their metadata from
// a metadata fake (the metadata-only lists), as one apiserver would.
func helmClients(t *testing.T, objs ...runtime.Object) (*kubefake.Clientset, *metadatafake.FakeMetadataClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	var metas []runtime.Object
	for _, o := range objs {
		var tm metav1.TypeMeta
		var om metav1.ObjectMeta
		switch v := o.(type) {
		case *corev1.Secret:
			tm, om = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, v.ObjectMeta
		case *corev1.ConfigMap:
			tm, om = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, v.ObjectMeta
		default:
			t.Fatalf("unsupported object %T", o)
		}
		metas = append(metas, &metav1.PartialObjectMetadata{TypeMeta: tm, ObjectMeta: om})
	}
	return kubefake.NewClientset(objs...), metadatafake.NewSimpleMetadataClient(scheme, metas...)
}

// collectHelmFrom runs collectHelm against objs with no KB lifecycle data.
func collectHelmFrom(t *testing.T, objs ...runtime.Object) (inventory.Inventory, error) {
	t.Helper()
	kube, meta := helmClients(t, objs...)
	var inv inventory.Inventory
	err := collectHelm(context.Background(), kube, meta, nil, &inv)
	return inv, err
}

func TestCollectHelmLatestRevisionPerRelease(t *testing.T) {
	inv, err := collectHelmFrom(t,
		helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 1, status: "superseded", chart: "ingress-nginx", chartVersion: "4.7.0", appVersion: "1.8.1"}),
		helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 2, status: "deployed", chart: "ingress-nginx", chartVersion: "4.7.1", appVersion: "1.8.4"}),
		helmSecret(t, helmRev{ns: "cert-manager", release: "cert-manager", rev: 1, status: "deployed", chart: "cert-manager", chartVersion: "v1.13.0", appVersion: "v1.13.0"}),
		&corev1.Secret{ // not a helm secret: must be ignored
			ObjectMeta: metav1.ObjectMeta{Name: "db-creds", Namespace: "shop"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"password": []byte("hunter2")},
		},
	)
	if err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	want := []inventory.HelmRelease{
		{Name: "cert-manager", Namespace: "cert-manager", ChartName: "cert-manager", ChartVersion: "v1.13.0", AppVersion: "v1.13.0", Status: "deployed", Revision: 1},
		{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.7.1", AppVersion: "1.8.4", Status: "deployed", Revision: 2},
	}
	if !reflect.DeepEqual(inv.HelmReleases, want) {
		t.Errorf("releases = %#v\nwant      %#v", inv.HelmReleases, want)
	}
}

// #24: listing is metadata-only and only the chosen revision's Secret is
// fetched and decoded, so memory no longer grows with release history.
func TestCollectHelmFetchesOnlyTheChosenRevision(t *testing.T) {
	var objs []runtime.Object
	for rev := 1; rev <= 10; rev++ {
		status := "superseded"
		if rev == 10 {
			status = "deployed"
		}
		for _, rel := range []string{"a", "b"} {
			objs = append(objs, helmSecret(t, helmRev{ns: "apps", release: rel, rev: rev, status: status, chart: rel, chartVersion: fmt.Sprintf("1.0.%d", rev)}))
		}
	}
	kube, meta := helmClients(t, objs...)
	var inv inventory.Inventory
	if err := collectHelm(context.Background(), kube, meta, nil, &inv); err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	var got []string
	for _, a := range kube.Actions() {
		switch a := a.(type) {
		case clienttesting.GetAction:
			got = append(got, a.GetVerb()+" "+a.GetResource().Resource+" "+a.GetName())
		default:
			got = append(got, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	slices.Sort(got)
	want := []string{"get secrets sh.helm.release.v1.a.v10", "get secrets sh.helm.release.v1.b.v10"}
	if !slices.Equal(got, want) {
		t.Errorf("typed-client requests = %v\nwant %v (lists must be metadata-only)", got, want)
	}
	if len(inv.HelmReleases) != 2 || inv.HelmReleases[0].ChartVersion != "1.0.10" || inv.HelmReleases[1].ChartVersion != "1.0.10" {
		t.Errorf("releases = %+v, want a and b at chart 1.0.10", inv.HelmReleases)
	}
	var listed []string
	for _, a := range meta.Actions() {
		if l, ok := a.(clienttesting.ListAction); ok {
			listed = append(listed, a.GetResource().Resource+"?"+l.GetListRestrictions().Labels.String())
		}
	}
	slices.Sort(listed)
	if want := []string{"configmaps?owner=helm", "secrets?owner=helm"}; !slices.Equal(listed, want) {
		t.Errorf("metadata lists = %v, want %v", listed, want)
	}
}

// Both drivers' metadata lists are paged, following the Continue token, so
// a cluster with thousands of release revisions never returns one unbounded
// list (PF-02 in docs/claims.md).
func TestCollectHelmFollowsListPagination(t *testing.T) {
	a := helmRev{ns: "apps", release: "a", rev: 1, status: "deployed", chart: "a", chartVersion: "1.0.0"}
	b := helmRev{ns: "apps", release: "b", rev: 1, status: "deployed", chart: "b", chartVersion: "2.0.0"}
	kube, meta := helmClients(t, helmSecret(t, a), helmSecret(t, b))
	item := func(r helmRev) runtime.RawExtension {
		return runtime.RawExtension{Object: &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: r.objectMeta()}}
	}
	for _, resource := range []string{"secrets", "configmaps"} {
		calls := 0
		meta.PrependReactor("list", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls > 2 {
				t.Fatalf("%s listed %d times, want 2 pages", resource, calls)
			}
			l := &metav1.List{}
			if calls == 1 {
				l.Continue = "page-2"
			}
			if resource == "secrets" {
				l.Items = []runtime.RawExtension{item([]helmRev{a, b}[calls-1])}
			}
			return true, l, nil
		})
	}

	var opts []metav1.ListOptions
	var inv inventory.Inventory
	if err := collectHelm(context.Background(), kube, recordingMeta{meta, &opts}, nil, &inv); err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	page := func(cont string) metav1.ListOptions {
		return metav1.ListOptions{LabelSelector: "owner=helm", Limit: listPageSize, Continue: cont}
	}
	if want := []metav1.ListOptions{page(""), page("page-2"), page(""), page("page-2")}; !reflect.DeepEqual(opts, want) {
		t.Errorf("list options = %+v\nwant %+v (both drivers paged, Continue token followed)", opts, want)
	}
	if len(inv.HelmReleases) != 2 {
		t.Errorf("releases = %+v, want a and b (releases from every page count)", inv.HelmReleases)
	}
}

// #25: which revision, if any, is installed. helm uninstall --keep-history
// marks the newest revision uninstalled and keeps the Secrets; a failed
// upgrade leaves the previous successful revision's resources running.
func TestCollectHelmInstalledRevision(t *testing.T) {
	rev := func(n int, status, chartVersion string) helmRev {
		return helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: n, status: status, chart: "ingress-nginx", chartVersion: chartVersion, appVersion: chartVersion}
	}
	cases := []struct {
		name string
		revs []helmRev
		want string // chart version of the release reported; "" = not installed
	}{
		{"newest uninstalled (--keep-history)", []helmRev{rev(3, "superseded", "4.7.0"), rev(4, "uninstalled", "4.8.0")}, ""},
		{"newest uninstalling", []helmRev{rev(3, "superseded", "4.7.0"), rev(4, "uninstalling", "4.8.0")}, ""},
		{"newest failed, earlier deployed", []helmRev{rev(1, "superseded", "4.6.0"), rev(2, "deployed", "4.7.0"), rev(3, "failed", "4.8.0")}, "4.7.0"},
		{"newest failed, earlier superseded", []helmRev{rev(1, "superseded", "4.6.0"), rev(2, "failed", "4.7.0"), rev(3, "failed", "4.8.0")}, "4.6.0"},
		{"failed install, nothing earlier", []helmRev{rev(1, "failed", "4.8.0")}, ""},
		{"newest pending-upgrade is present", []helmRev{rev(1, "deployed", "4.7.0"), rev(2, "pending-upgrade", "4.8.0")}, "4.8.0"},
		{"newest pending-install is present", []helmRev{rev(1, "pending-install", "4.8.0")}, "4.8.0"},
		{"newest pending-rollback is present", []helmRev{rev(1, "superseded", "4.7.0"), rev(2, "superseded", "4.8.0"), rev(3, "pending-rollback", "4.7.0")}, "4.7.0"},
		{"revision 10 is newer than revision 9", []helmRev{rev(9, "superseded", "4.7.0"), rev(10, "deployed", "4.8.0")}, "4.8.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			for _, r := range tc.revs {
				objs = append(objs, helmSecret(t, r))
			}
			inv, err := collectHelmFrom(t, objs...)
			if err != nil && !errors.As(err, new(partialError)) {
				t.Fatal(err)
			}
			switch {
			case tc.want == "" && len(inv.HelmReleases) != 0:
				t.Errorf("releases = %+v, want none: nothing is installed", inv.HelmReleases)
			case tc.want != "" && (len(inv.HelmReleases) != 1 || inv.HelmReleases[0].ChartVersion != tc.want):
				t.Errorf("releases = %+v, want one at chart %s", inv.HelmReleases, tc.want)
			}
		})
	}
}

// #25 end to end: ingress-nginx removed with --keep-history leaves no
// add-on and no EOL blocker.
func TestUninstalledHelmReleaseRaisesNoEOLBlocker(t *testing.T) {
	inv, err := collectHelmFrom(t,
		helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 3, status: "superseded", chart: "ingress-nginx", chartVersion: "4.11.3", appVersion: "1.11.3"}),
		helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 4, status: "uninstalled", chart: "ingress-nginx", chartVersion: "4.11.3", appVersion: "1.11.3"}),
	)
	if err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	inv.AddOns, _ = matchAddOns(addOnEvidence{releases: inv.HelmReleases}, addons)
	inv.ServerVersion = "v1.33.1"
	inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapVersions: {Available: true}, inventory.CapAPIUsage: {Available: true},
		inventory.CapHelm: {Available: true}, inventory.CapAddOns: {Available: true},
	}
	k := kb.KB{AddOns: addons, Skew: kb.DefaultSkewPolicy(), MaxKnownK8s: inventory.Version{Major: 1, Minor: 99}}
	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 34}, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if len(inv.AddOns) != 0 || len(rep.Findings) != 0 || !rep.Ready {
		t.Errorf("add-ons %+v, findings %+v, ready %v; want none, none, true", inv.AddOns, rep.Findings, rep.Ready)
	}
}

func TestCollectHelmSkipsCorruptSecretKeepsValid(t *testing.T) {
	corrupt := helmSecret(t, helmRev{ns: "shop", release: "broken", rev: 1, status: "deployed"})
	corrupt.Data["release"] = []byte("%%% not base64 %%%") // helm-labelled but corrupt payload: skipped, not fatal
	inv, err := collectHelmFrom(t,
		helmSecret(t, helmRev{ns: "cert-manager", release: "cert-manager", rev: 1, status: "deployed", chart: "cert-manager", chartVersion: "v1.13.0", appVersion: "v1.13.0"}),
		corrupt,
		helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 1, status: "deployed", chart: "ingress-nginx", chartVersion: "4.7.1", appVersion: "1.8.4"}),
	)
	var pe partialError
	if !errors.As(err, &pe) {
		t.Fatalf("one corrupt secret must not fail the capability: %v", err)
	}
	// Skipped, but counted: an undecodable release may be the EOL add-on.
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"shop/broken"}) ||
		!strings.Contains(pe.msg, "1 release(s) not decodable, first shop/broken: ") {
		t.Errorf("partial = %v, skipped = %q, reason = %q; want incomplete, skipping and counting shop/broken", pe.incomplete, pe.skipped, pe.msg)
	}
	want := []inventory.HelmRelease{
		{Name: "cert-manager", Namespace: "cert-manager", ChartName: "cert-manager", ChartVersion: "v1.13.0", AppVersion: "v1.13.0", Status: "deployed", Revision: 1},
		{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.7.1", AppVersion: "1.8.4", Status: "deployed", Revision: 1},
	}
	if !reflect.DeepEqual(inv.HelmReleases, want) {
		t.Errorf("releases = %#v\nwant      %#v (valid secrets must survive the corrupt one)", inv.HelmReleases, want)
	}
}

func TestDecodeHelmReleaseRejectsNonGzip(t *testing.T) {
	if _, err := decodeHelmRelease([]byte(base64.StdEncoding.EncodeToString([]byte("plain")))); err == nil {
		t.Fatal("want error for payload without gzip magic bytes")
	}
}

// helmBomb is a Helm release payload, base64(gzip(prefix + n bytes of fill
// repeated + suffix)), whose stored size is a tiny fraction of what it
// decodes to: 700 MiB of whitespace fits a 951 KB Secret (#168).
func helmBomb(t testing.TB, prefix, fill string, n int, suffix string) []byte {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(prefix))
	chunk := bytes.Repeat([]byte(fill), 1<<20/max(1, len(fill)))
	for left := n; left > 0; left -= len(chunk) {
		zw.Write(chunk[:min(left, len(chunk))])
	}
	zw.Write([]byte(suffix))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))
}

// A payload that decompresses past maxReleaseJSONBytes is refused as too
// large, whether its JSON is valid (a release with a huge manifest) or not
// (whitespace), and one that decodes to exactly the cap still decodes.
func TestDecodeHelmReleaseBoundsDecompressedSize(t *testing.T) {
	const head, tail = `{"chart":{"metadata":{"name":"bomb"}},"manifest":"`, `"}`
	fits := maxReleaseJSONBytes - len(head) - len(tail)
	for name, tc := range map[string]struct {
		payload []byte
		tooBig  bool
	}{
		"valid json at the cap":   {helmBomb(t, head, "a", fits, tail), false},
		"valid json over the cap": {helmBomb(t, head, "a", fits+1, tail), true},
		"whitespace over the cap": {helmBomb(t, "", " ", maxReleaseJSONBytes+1, ""), true},
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := decodeHelmRelease(tc.payload)
			if tc.tooBig {
				if !errors.Is(err, errReleaseTooLarge) || !strings.Contains(err.Error(), "release payload too large: over 16 MiB decompressed") {
					t.Errorf("err = %v, want errReleaseTooLarge naming the 16 MiB cap", err)
				}
				return
			}
			if err != nil || doc.Chart.Metadata.Name != "bomb" || len(doc.Manifest) != fits {
				t.Errorf("chart %q, manifest of %d bytes, err %v; want bomb, %d bytes, nil", doc.Chart.Metadata.Name, len(doc.Manifest), err, fits)
			}
		})
	}
}

// The whole gzip stream is read and checked, not only the JSON value at
// its start: a payload whose CRC or size trailer is wrong (corrupt, or a
// size that lies about the buffer the JSON needs), whose JSON is followed
// by more, or that holds more than one gzip member (Helm writes one) or
// bytes after it, is not decodable, as when it was read with ReadAll.
func TestDecodeHelmReleaseChecksTheWholeStream(t *testing.T) {
	gz := func(s string) []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write([]byte(s))
		zw.Close()
		return b.Bytes()
	}
	const doc = `{"chart":{"metadata":{"name":"x"}}}`
	corrupt := func(at int) []byte { // flips a trailer byte: -8 CRC, -4 size
		b := gz(doc)
		b[len(b)+at] ^= 0xff
		return b
	}
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"bad crc":        {corrupt(-8), "gunzip read: gzip: invalid checksum"},
		"lying size":     {corrupt(-4), "gunzip read: gzip: invalid checksum"},
		"trailing data":  {gz(doc + `{"more":1}`), "release json: invalid character '{' after top-level value"},
		"second member":  {append(gz(doc), gz("")...), "gunzip: decompresses past its size trailer"},
		"trailing bytes": {append(gz(doc), 0xff, 0xff, 0xff, 0), "gunzip: data after the gzip stream"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeHelmRelease([]byte(base64.StdEncoding.EncodeToString(tc.raw)))
			if err == nil || err.Error() != tc.want {
				t.Errorf("err = %v, want %s", err, tc.want)
			}
		})
	}
	if d, err := decodeHelmRelease([]byte(base64.StdEncoding.EncodeToString(gz(doc)))); err != nil || d.Chart.Metadata.Name != "x" {
		t.Errorf("intact payload: %+v, %v", d, err)
	}
}

// The stored payload is bounded before it is base64-decoded: nothing
// larger fits a Secret or ConfigMap, so only a misbehaving apiserver could
// serve one.
func TestDecodeHelmReleaseBoundsStoredSize(t *testing.T) {
	big := bytes.Repeat([]byte("A"), maxStoredReleaseBytes+4)
	if _, err := decodeHelmRelease(big); !errors.Is(err, errReleaseTooLarge) || !strings.Contains(err.Error(), "over 4 MiB stored") {
		t.Errorf("err = %v, want errReleaseTooLarge naming the 4 MiB stored cap", err)
	}
}

// The chart's kubeVersion constraint is kept verbatim, and the stored
// manifest is parsed with the --files parser: objects at APIs the KB flags
// are kept per GVK with their name, manifest line and template; others are
// dropped.
func TestCollectHelmChartKubeVersionAndManifestAPIs(t *testing.T) {
	manifest := `---
# Source: apf/templates/configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
---
# Source: apf/templates/flowschema.yaml
apiVersion: flowcontrol.apiserver.k8s.io/v1beta3
kind: FlowSchema
metadata:
  name: batch-jobs
`
	kube, meta := helmClients(t, helmSecret(t, helmRev{ns: "platform", release: "apf", rev: 2, status: "deployed",
		chart: "apf", chartVersion: "0.3.0", kubeVersion: ">=1.21.0-0 <1.33.0-0", manifest: manifest}))
	v := func(m int) *inventory.Version { return &inventory.Version{Major: 1, Minor: m} }
	lifecycle := []kb.APILifecycleEntry{
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Introduced: *v(26), Deprecated: v(29), Removed: v(32)},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "FlowSchema", Introduced: *v(29)},
	}
	var inv inventory.Inventory
	if err := collectHelm(context.Background(), kube, meta, lifecycle, &inv); err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	want := []inventory.HelmRelease{{
		Name: "apf", Namespace: "platform", ChartName: "apf", ChartVersion: "0.3.0",
		KubeVersion: ">=1.21.0-0 <1.33.0-0", Status: "deployed", Revision: 2,
		ManifestAPIs: []inventory.APIUsage{{
			Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Count: 1,
			Namespaces: map[string]int{"": 1},
			Objects:    []inventory.ObjectRef{{Name: "batch-jobs", Line: 9, RenderedFrom: "apf/templates/flowschema.yaml"}},
		}},
	}}
	if !reflect.DeepEqual(inv.HelmReleases, want) {
		t.Errorf("releases = %#v\nwant      %#v", inv.HelmReleases, want)
	}
}

// A manifest parsed a run of documents at a time (#168) yields what it
// yields parsed whole: the same objects, counts and manifest lines. The
// manifests span several runs, one with a document larger than a run, one
// that stops at an invalid separator as the parser does, and one in JSON,
// which is not split.
func TestManifestAPIsInRunsMatchesWholeParse(t *testing.T) {
	flagged := map[gvk]bool{{"", "v1", "ConfigMap"}: true}
	var b strings.Builder
	for i := range 90 {
		fmt.Fprintf(&b, "--- # Source: big/templates/cm-%d.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm-%d\n  namespace: ns-%d\n", i, i, i%3)
		fmt.Fprintf(&b, "---\n# filler\napiVersion: v1\nkind: Secret\nmetadata:\n  name: s-%d\ndata:\n", i)
		pad := 400 // lines of 60 bytes and 2 YAML nodes: 23 KiB
		if i == 45 {
			pad = 25000 // 1.4 MiB and 50,000 nodes: one document over a run
		}
		for j := range pad {
			fmt.Fprintf(&b, "  k%06d: dmFsdWUgdmFsdWUgdmFsdWUgdmFsdWUgdmFsdWUgdmFsdWUK\n", j)
		}
	}
	yamlManifest := b.String()
	half := len(yamlManifest) / 2
	cut := half + strings.Index(yamlManifest[half:], "\n---")
	for name, manifest := range map[string]string{
		"yaml":              yamlManifest,
		"invalid separator": yamlManifest[:cut+1] + "--- not a separator\n" + yamlManifest[cut+1:],
		"json":              `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}` + "\n" + `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"b"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			objs, _, _, _ := parseManifestStream(strings.NewReader(manifest))
			counts := map[gvk]*inventory.APIUsage{}
			accumulate(counts, slices.DeleteFunc(objs, func(o manifestObject) bool { return !flagged[gvk{o.group, o.version, o.kind}] }))
			want := usageRows(counts)
			runs := 0
			splitManifest(manifest, func(string, int, bool) { runs++ })
			got, err := manifestAPIs(manifest, flagged)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("in %d runs: %+v, %v\nwhole:     %+v", runs, got, err, want)
			}
			if name != "json" && runs < 3 {
				t.Errorf("%d runs, want several", runs)
			}
		})
	}
}

// splitManifest's runs are as large as the bounds let them be and no
// larger (#168, #213): 1 MiB and 64Ki YAML nodes, written here as numbers,
// not as the constants, so changing manifestChunkBytes or maxManifestNodes
// fails this test whatever the heap tests read. Those cannot see a byte
// bound of 4 MiB instead of 1: the release itself is most of their figure.
// Each manifest is of identical documents, one bound by bytes and one by
// nodes, so every run but the last holds exactly as many documents as fit
// the bound that binds it, and one more would not.
func TestSplitManifestRunsHoldTheBounds(t *testing.T) {
	const runBytes, runNodes = 1 << 20, 1 << 16
	for _, tc := range []struct {
		name, body string // a document, after its "---" line
	}{
		{"bound by bytes", "apiVersion: v1\nkind: ConfigMap\ndata:\n  k: " + strings.Repeat("x", 4000) + "\n"},
		{"bound by nodes", "apiVersion: v1\nkind: ConfigMap\ndata:\n" + strings.Repeat(" k: v\n", 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := "---\n" + tc.body
			nodes := 2 + yamlNodeBound(tc.body) // its root and document nodes, and what its lines bound
			perRun := min(runBytes/len(doc), runNodes/nodes)
			const docs = 2000
			if perRun >= docs/3 {
				t.Fatalf("%d documents a run: the manifest must span several runs", perRun)
			}
			var runs []int // documents in each run
			splitManifest(strings.Repeat(doc, docs), func(text string, line int, whole bool) {
				n := strings.Count(text, "---\n")
				if !whole || len(text) != n*len(doc) {
					t.Fatalf("run %d at line %d: whole %v, %d bytes of %d documents; want whole documents", len(runs), line, whole, len(text), n)
				}
				if len(text) > runBytes || n*nodes > runNodes {
					t.Errorf("run %d: %d bytes and %d nodes, over %d bytes or %d nodes", len(runs), len(text), n*nodes, runBytes, runNodes)
				}
				runs = append(runs, n)
			})
			if len(runs) < 2 {
				t.Fatalf("%d runs, want several", len(runs))
			}
			total := 0
			for i, n := range runs {
				total += n
				if i < len(runs)-1 && n != perRun {
					t.Errorf("run %d holds %d documents, want %d: as many as fit %d bytes and %d nodes", i, n, perRun, runBytes, runNodes)
				}
			}
			if total != docs {
				t.Errorf("runs hold %d documents, want %d", total, docs)
			}
		})
	}
}

// A document over maxManifestDocBytes or maxManifestNodes is not parsed,
// whatever it holds (#168): the release is still recorded with the objects
// of its other documents, and the capability is Partial, naming the
// release and the first document's line.
func TestCollectHelmManifestDocumentOverTheBoundsIsAGap(t *testing.T) {
	const cm = "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n"
	manifest := fmt.Sprintf(cm, "first") +
		fmt.Sprintf(cm, "too-long") + strings.Repeat("# comment\n", maxManifestDocBytes/10) +
		fmt.Sprintf(cm, "too-many-nodes") + "data:\n" + strings.Repeat(" k: v\n", maxManifestNodes/2) +
		fmt.Sprintf(cm, "last")
	kube, meta := helmClients(t, helmSecret(t, helmRev{ns: "a-bomb", release: "bomb", rev: 1, status: "deployed", chart: "bomb", manifest: manifest}))
	lifecycle := []kb.APILifecycleEntry{{Version: "v1", Kind: "ConfigMap", Deprecated: &inventory.Version{Major: 1, Minor: 99}}}
	var inv inventory.Inventory
	err := collectHelm(context.Background(), kube, meta, lifecycle, &inv)
	var pe partialError
	if !errors.As(err, &pe) || !pe.incomplete || !slices.Equal(pe.skipped, []string{"a-bomb/bomb"}) ||
		!strings.HasSuffix(pe.msg, "; 1 release manifest(s) not fully parsed, first a-bomb/bomb: manifest document too large: 2 document(s) over 2 MiB or 65536 YAML nodes not parsed, first at manifest line 6") {
		t.Fatalf("err = %v (skipped %q), want incomplete, naming a-bomb/bomb and the documents not parsed", err, pe.skipped)
	}
	if len(inv.HelmReleases) != 1 {
		t.Fatalf("releases = %+v, want bomb", inv.HelmReleases)
	}
	var names []string
	for _, u := range inv.HelmReleases[0].ManifestAPIs {
		for _, o := range u.Objects {
			names = append(names, o.Name)
		}
	}
	if !slices.Equal(names, []string{"first", "last"}) {
		t.Errorf("objects = %q, want first and last: the documents around those not parsed", names)
	}
}

// #26 end to end, with the embedded KB: on a cluster that no longer serves
// flowcontrol v1beta3 (so the live scan sees nothing), a release whose
// stored manifest holds a v1beta3 FlowSchema blocks, and the report names
// the release.
func TestHelmManifestWithRemovedAPIBlocksEndToEnd(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	kube, meta := helmClients(t, helmSecret(t, helmRev{ns: "platform", release: "apf", rev: 4, status: "deployed", chart: "apf", chartVersion: "0.3.0",
		manifest: "# Source: apf/templates/fs.yaml\napiVersion: flowcontrol.apiserver.k8s.io/v1beta3\nkind: FlowSchema\nmetadata:\n  name: batch-jobs\n"}))
	inv := inventory.Inventory{ServerVersion: "v1.32.4", Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapVersions: {Available: true}, inventory.CapAPIUsage: {Available: true},
	}}
	if err := collectHelm(context.Background(), kube, meta, k.APILifecycle, &inv); err != nil && !errors.As(err, new(partialError)) {
		t.Fatal(err)
	}
	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 33}, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if rep.Ready || len(rep.Findings) != 1 || rep.Findings[0].Severity != engine.SevBlocker {
		t.Fatalf("ready %v, findings %+v; want one blocker", rep.Ready, rep.Findings)
	}
	out, err := json.Marshal(rep.Findings[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"key":"removed-api/helm-release/platform/apf"`, "release platform/apf", `"name":"batch-jobs"`, "flowcontrol.apiserver.k8s.io/v1beta3 FlowSchema"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("finding JSON %s\nmissing %s", out, want)
		}
	}
	// SARIF has no file to anchor it to, so it is a notification that
	// still names the release.
	var doc bytes.Buffer
	if err := sarif.Write(&doc, rep, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"findingKey": "removed-api/helm-release/platform/apf"`, "helm upgrade of release platform/apf will fail"} {
		if !strings.Contains(doc.String(), want) {
			t.Errorf("SARIF %s\nmissing %s", doc.String(), want)
		}
	}
}

// #70: releases stored by Helm's configmap driver (HELM_DRIVER=configmap)
// are read too, and the capability reason says how many came from where.
func TestCollectHelmReadsConfigMapDriver(t *testing.T) {
	inv, err := collectHelmFrom(t,
		helmSecret(t, helmRev{ns: "a", release: "from-secret", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}),
		helmConfigMap(t, helmRev{ns: "b", release: "from-cm", rev: 1, status: "superseded", chart: "y", chartVersion: "2.0.0"}),
		helmConfigMap(t, helmRev{ns: "b", release: "from-cm", rev: 2, status: "deployed", chart: "y", chartVersion: "2.1.0"}),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "b"}, Data: map[string]string{"ca.crt": "x"}},
	)
	var pe partialError
	if !errors.As(err, &pe) || pe.Error() != "helm releases: 1 via secrets, 1 via configmaps" || pe.incomplete {
		t.Errorf("err = %#v, want a complete (informational) partialError %q", err, "helm releases: 1 via secrets, 1 via configmaps")
	}
	want := []inventory.HelmRelease{
		{Name: "from-secret", Namespace: "a", ChartName: "x", ChartVersion: "1.0.0", Status: "deployed", Revision: 1},
		{Name: "from-cm", Namespace: "b", ChartName: "y", ChartVersion: "2.1.0", Status: "deployed", Revision: 2},
	}
	if !reflect.DeepEqual(inv.HelmReleases, want) {
		t.Errorf("releases = %#v\nwant      %#v", inv.HelmReleases, want)
	}
}

// A release with history in both drivers (HELM_DRIVER changed between
// installs) is read from the secrets driver, Helm's default, even when the
// configmaps history is newer by number: revision numbers of two separate
// histories are not comparable, and a stale higher one would otherwise win.
func TestCollectHelmPrefersSecretsDriver(t *testing.T) {
	inv, err := collectHelmFrom(t,
		helmSecret(t, helmRev{ns: "a", release: "r", rev: 2, status: "deployed", chart: "x", chartVersion: "2.0.0"}),
		helmConfigMap(t, helmRev{ns: "a", release: "r", rev: 7, status: "deployed", chart: "x", chartVersion: "1.0.0"}),
		helmConfigMap(t, helmRev{ns: "a", release: "r", rev: 2, status: "superseded", chart: "x", chartVersion: "0.9.0"}),
	)
	var pe partialError
	if !errors.As(err, &pe) || pe.Error() != "helm releases: 1 via secrets, 0 via configmaps" {
		t.Errorf("err = %v, want partialError %q", err, "helm releases: 1 via secrets, 0 via configmaps")
	}
	want := []inventory.HelmRelease{{Name: "r", Namespace: "a", ChartName: "x", ChartVersion: "2.0.0", Status: "deployed", Revision: 2}}
	if !reflect.DeepEqual(inv.HelmReleases, want) {
		t.Errorf("releases = %#v\nwant      %#v", inv.HelmReleases, want)
	}
}

// Each storage driver degrades on its own: a forbidden ConfigMap list
// (a role that grants only Secrets) keeps the Secret releases; only when
// every driver fails is the capability unavailable.
func TestCollectHelmDegradesPerDriver(t *testing.T) {
	forbid := func(meta *metadatafake.FakeMetadataClient, resource string) {
		meta.PrependReactor("list", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("RBAC"))
		})
	}
	secret := helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"})

	kube, meta := helmClients(t, secret)
	forbid(meta, "configmaps")
	var inv inventory.Inventory
	err := collectHelm(context.Background(), kube, meta, nil, &inv)
	var pe partialError
	if !errors.As(err, &pe) || !strings.HasPrefix(pe.Error(), "helm releases: 1 via secrets; configmaps not read: ") || !strings.Contains(pe.Error(), "forbidden") {
		t.Errorf("err = %v, want a partialError counting the Secret release and naming the forbidden ConfigMap list", err)
	}
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"configmaps"}) {
		t.Errorf("partial = %v, skipped = %q; want incomplete, skipping the configmaps driver", pe.incomplete, pe.skipped)
	}
	if len(inv.HelmReleases) != 1 {
		t.Errorf("releases = %+v, want the Secret release", inv.HelmReleases)
	}

	kube, meta = helmClients(t, secret)
	forbid(meta, "configmaps")
	forbid(meta, "secrets")
	inv = inventory.Inventory{}
	err = collectHelm(context.Background(), kube, meta, nil, &inv)
	if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "secrets") || !strings.Contains(err.Error(), "configmaps") {
		t.Errorf("err = %v, want a full failure naming both drivers", err)
	}

	// Secrets (Helm's default driver) forbidden, ConfigMaps readable but
	// empty — the built-in view ClusterRole. Zero releases here says nothing
	// about the cluster, so the capability must be unavailable (a gap in the
	// report), not available with an unrendered reason.
	kube, meta = helmClients(t)
	forbid(meta, "secrets")
	inv = inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
	runSteps(context.Background(), &inv, []step{{cap: inventory.CapHelm, run: func(ctx context.Context, inv *inventory.Inventory) error {
		return collectHelm(ctx, kube, meta, nil, inv)
	}}})
	if st := inv.Capabilities[inventory.CapHelm]; st.Available || !strings.Contains(st.Reason, "0 via configmaps") || !strings.Contains(st.Reason, "secrets not read: ") {
		t.Errorf("helm capability = %+v, want unavailable, naming both drivers", st)
	}

	// Secrets forbidden, a ConfigMap release readable: the release is kept
	// (reads degrade per driver), but the default driver went unread, so
	// the capability is still a gap.
	cmRelease := helmConfigMap(t, helmRev{ns: "b", release: "c", rev: 1, status: "deployed", chart: "y", chartVersion: "2.0.0"})
	kube, meta = helmClients(t, cmRelease)
	forbid(meta, "secrets")
	inv = inventory.Inventory{}
	err = collectHelm(context.Background(), kube, meta, nil, &inv)
	if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "1 via configmaps") || !strings.Contains(err.Error(), "secrets not read: ") {
		t.Errorf("err = %v, want a full failure naming both drivers", err)
	}
	if len(inv.HelmReleases) != 1 || inv.HelmReleases[0].Name != "c" {
		t.Errorf("releases = %+v, want the ConfigMap release kept", inv.HelmReleases)
	}

	// ConfigMaps forbidden and no Secret releases: nothing read while a
	// driver failed is not "no releases".
	kube, meta = helmClients(t)
	forbid(meta, "configmaps")
	inv = inventory.Inventory{}
	err = collectHelm(context.Background(), kube, meta, nil, &inv)
	if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "0 via secrets") || !strings.Contains(err.Error(), "configmaps not read: ") {
		t.Errorf("err = %v, want a full failure naming both drivers", err)
	}

	// A role with list but not get: releases are seen but none can be read.
	kube, meta = helmClients(t, secret)
	kube.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "sh.helm.release.v1.r.v1", errors.New("RBAC"))
	})
	inv = inventory.Inventory{}
	err = collectHelm(context.Background(), kube, meta, nil, &inv)
	if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "1 release(s) not read, first a/r: ") {
		t.Errorf("err = %v, want a full failure: no listed release could be read", err)
	}
}
