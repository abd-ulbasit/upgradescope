package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// decodeRelease reverses Helm's storage encoding the way the collector does:
// base64, then gunzip, then JSON.
func decodeRelease(t *testing.T, stored []byte) map[string]any {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(string(stored))
	if err != nil {
		t.Fatalf("release payload is not base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("release payload is not gzip: %v", err)
	}
	js, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(js, &doc); err != nil {
		t.Fatalf("release payload is not JSON: %v", err)
	}
	return doc
}

func TestHelmReleaseSecretIsAHelmReleaseSecret(t *testing.T) {
	cfg := config{Namespaces: 10, HelmRevisions: 2, Seed: 1}
	rel, err := helmReleaseSecrets(7, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.secrets) != 2 {
		t.Fatalf("secrets = %d, want one per revision (2)", len(rel.secrets))
	}
	for n, s := range rel.secrets {
		rev := n + 1
		wantStatus := map[int]string{1: "superseded", 2: "deployed"}[rev]
		if s.Type != "helm.sh/release.v1" || s.Labels["owner"] != "helm" || s.Labels["name"] != "rel-0007" ||
			s.Labels["status"] != wantStatus || s.Labels["version"] != string(rune('0'+rev)) {
			t.Errorf("revision %d: type %q labels %v, want a helm.sh/release.v1 Secret labelled owner=helm name=rel-0007 status=%s version=%d", rev, s.Type, s.Labels, wantStatus, rev)
		}
		if want := "sh.helm.release.v1.rel-0007.v" + string(rune('0'+rev)); s.Name != want {
			t.Errorf("name = %q, want %q", s.Name, want)
		}
		doc := decodeRelease(t, s.Data["release"])
		meta := doc["chart"].(map[string]any)["metadata"].(map[string]any)
		if meta["name"] == "" || meta["version"] == "" || meta["appVersion"] == "" {
			t.Errorf("chart metadata incomplete: %v", meta)
		}
		if doc["info"].(map[string]any)["status"] != wantStatus {
			t.Errorf("info.status = %v, want %s", doc["info"], wantStatus)
		}
		m := doc["manifest"].(string)
		if !strings.Contains(m, "kind: Deployment") || !strings.Contains(m, "\n---\n") {
			t.Errorf("manifest is not a multi-document YAML of objects:\n%.200s", m)
		}
	}
	if rel.gzSize != len(rel.secrets[1].Data["release"]) {
		t.Errorf("gzSize = %d, want the installed revision's stored size %d", rel.gzSize, len(rel.secrets[1].Data["release"]))
	}
}

func TestHelmReleasesAreDeterministic(t *testing.T) {
	cfg := config{Namespaces: 10, HelmRevisions: 1, Seed: 42}
	a, _ := helmReleaseSecrets(33, cfg)
	b, _ := helmReleaseSecrets(33, cfg)
	if !bytes.Equal(a.secrets[0].Data["release"], b.secrets[0].Data["release"]) {
		t.Error("the same seed and index gave different payloads")
	}
	cfg.Seed = 43
	c, _ := helmReleaseSecrets(33, cfg)
	if bytes.Equal(a.secrets[0].Data["release"], c.secrets[0].Data["release"]) {
		t.Error("another seed gave the same payload")
	}
}

func TestHelmSizeClassesAndRealisticSizes(t *testing.T) {
	cfg := config{Namespaces: 100, HelmRevisions: 1, Seed: 1}
	counts := map[string]int{}
	sizes := map[string][]int{}
	flagged := 0
	for i := range 200 {
		rel, err := helmReleaseSecrets(i, cfg)
		if err != nil {
			t.Fatal(err)
		}
		counts[rel.class]++
		sizes[rel.class] = append(sizes[rel.class], rel.gzSize)
		if strings.Contains(string(decodeManifest(t, rel)), "PodSecurityPolicy") {
			flagged++
		}
	}
	if counts["small"] != 160 || counts["medium"] != 30 || counts["large"] != 10 {
		t.Errorf("class mix over 200 releases = %v, want 160 small, 30 medium, 10 large", counts)
	}
	if flagged != 4 {
		t.Errorf("%d releases hold a flagged API object, want 1 in %d (4 of 200)", flagged, deprecatedEvery)
	}
	avg := func(xs []int) int {
		s := 0
		for _, x := range xs {
			s += x
		}
		return s / len(xs)
	}
	// Stored sizes grow with the class and stay under the 1 MiB a Secret
	// can hold, with the large ones in the hundreds of KiB.
	small, medium, large := avg(sizes["small"]), avg(sizes["medium"]), avg(sizes["large"])
	if !(small < medium && medium < large) {
		t.Errorf("average stored sizes small %d, medium %d, large %d: want them to grow", small, medium, large)
	}
	if large > 900<<10 || large < 50<<10 {
		t.Errorf("large release stored at %d KiB on average, want between 50 and 900 KiB", large>>10)
	}
	if small < 1<<10 {
		t.Errorf("small release stored at %d bytes, too small to be a chart", small)
	}
}

// docs/operations/scale.md says that, at the default 100 namespaces and 1,000
// releases, the large releases are exactly the last five namespaces
// (bench-ns-095 to bench-ns-099), the last 50 releases in the order the Helm
// step visits them (namespace, then name).
func TestLargeReleasesAreTheLastFiveNamespaces(t *testing.T) {
	cfg := config{Namespaces: 100, HelmRevisions: 1, Seed: 1}
	type rel struct{ ns, name, class string }
	var all []rel
	for i := range 1000 {
		r, err := helmReleaseSecrets(i, cfg)
		if err != nil {
			t.Fatal(err)
		}
		s := r.secrets[0]
		all = append(all, rel{s.Namespace, s.Labels["name"], r.class})
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].ns != all[b].ns {
			return all[a].ns < all[b].ns
		}
		return all[a].name < all[b].name
	})
	for i, r := range all {
		wantLarge := i >= len(all)-50
		if (r.class == "large") != wantLarge {
			t.Fatalf("release %d in visit order (%s/%s) is %s, want large exactly for the last 50", i, r.ns, r.name, r.class)
		}
		if wantLarge && (r.ns < "bench-ns-095" || r.ns > "bench-ns-099") {
			t.Errorf("large release %s/%s is outside bench-ns-095 to bench-ns-099", r.ns, r.name)
		}
	}
	if first := all[len(all)-50].ns; first != "bench-ns-095" {
		t.Errorf("the last 50 releases start in %s, want bench-ns-095", first)
	}
}

func decodeManifest(t *testing.T, rel helmRelease) []byte {
	t.Helper()
	return []byte(decodeRelease(t, rel.secrets[len(rel.secrets)-1].Data["release"])["manifest"].(string))
}

func TestNodeObjectIsAKwokNodeWithAKubeletVersion(t *testing.T) {
	n := nodeObject(12, "v1.37.0")
	if n.Name != "kwok-node-00012" || n.Annotations[kwokNodeAnnotation] != kwokNodeValue {
		t.Errorf("node %q annotations %v: KWOK manages only annotated nodes", n.Name, n.Annotations)
	}
	if n.Status.NodeInfo.KubeletVersion != "v1.37.0" || n.Status.NodeInfo.ContainerRuntimeVersion == "" {
		t.Errorf("nodeInfo = %+v, want the kubelet and runtime versions KWOK keeps", n.Status.NodeInfo)
	}
	if len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("taints = %v, want a NoSchedule taint so the real scheduler leaves the node alone", n.Spec.Taints)
	}
	if n.Labels[benchLabel] != "true" {
		t.Errorf("labels = %v, want the bench label", n.Labels)
	}
}

func TestPodsSpreadOverNodesAndNamespacesAndCarryAddOns(t *testing.T) {
	cfg := config{Nodes: 20, Namespaces: 5}
	nodes, nss, addons := map[string]bool{}, map[string]bool{}, 0
	for i := range 1000 {
		p := podObject(i, cfg)
		nodes[p.Spec.NodeName] = true
		nss[p.Namespace] = true
		if p.Spec.NodeName == "" || len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Image == "" {
			t.Fatalf("pod %d: nodeName %q containers %v", i, p.Spec.NodeName, p.Spec.Containers)
		}
		if p.Labels["app.kubernetes.io/name"] != "" {
			addons++
		}
	}
	if len(nodes) != 20 || len(nss) != 5 {
		t.Errorf("pods use %d nodes and %d namespaces, want 20 and 5", len(nodes), len(nss))
	}
	if addons != 1000/addOnEvery {
		t.Errorf("%d add-on pods in 1000, want %d", addons, 1000/addOnEvery)
	}
}

func TestRunCreatesEveryObjectAndIsRerunnable(t *testing.T) {
	cfg := config{Nodes: 5, Namespaces: 3, Pods: 7, ConfigMaps: 4, Deployments: 3, HelmReleases: 6, HelmRevisions: 2, KubeletVersion: "v1.37.0", Seed: 1}
	cs := fake.NewClientset()
	var log bytes.Buffer
	for pass := range 2 { // the second pass meets what the first made
		sum, err := run(context.Background(), cs, cfg, 4, 0, &log)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if sum.Nodes != 5 || sum.Pods != 7 || sum.HelmReleases != 6 || sum.Namespaces != 3 || sum.ConfigMaps != 4 || sum.Deployments != 3 {
			t.Errorf("pass %d: summary %+v", pass, sum)
		}
	}
	list := func(n int, err error) int {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	ctx := context.Background()
	nodes, _ := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	pods, _ := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	secrets, _ := cs.CoreV1().Secrets("").List(ctx, metav1.ListOptions{})
	cms, _ := cs.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{})
	deps, _ := cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	for what, got := range map[string][2]int{
		"nodes": {len(nodes.Items), 5}, "pods": {len(pods.Items), 7}, "helm secrets (6 releases x 2 revisions)": {len(secrets.Items), 12},
		"configmaps": {len(cms.Items), 4}, "deployments": {len(deps.Items), 3},
	} {
		if list(got[0], nil) != got[1] {
			t.Errorf("%s = %d, want %d", what, got[0], got[1])
		}
	}
}

func TestRunStopsAtTheFirstFailure(t *testing.T) {
	cs := fake.NewClientset()
	boom := errors.New("etcd is full")
	cs.PrependReactor("create", "nodes", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, boom })
	_, err := run(context.Background(), cs, config{Nodes: 50, Namespaces: 1, Pods: 5, KubeletVersion: "v1.37.0"}, 4, 0, io.Discard)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "nodes") {
		t.Fatalf("err = %v, want the node create failure named", err)
	}
	pods, _ := cs.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("%d pods created after the nodes failed, want none", len(pods.Items))
	}
}

func TestParallelRunsEveryIndexOnceAndReturnsAnError(t *testing.T) {
	var seen [100]atomic.Int32
	if err := parallel(context.Background(), 100, 8, func(_ context.Context, i int) error { seen[i].Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	for i := range seen {
		if seen[i].Load() != 1 {
			t.Fatalf("index %d ran %d times, want once", i, seen[i].Load())
		}
	}
	want := errors.New("no")
	err := parallel(context.Background(), 100, 8, func(_ context.Context, i int) error {
		if i == 3 {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

func TestAwaitKWOKCountsReadyNodesAndRunningPods(t *testing.T) {
	cs := fake.NewClientset()
	ctx := context.Background()
	for i := range 3 {
		n := nodeObject(i, "v1.37.0")
		if i < 2 { // the third node is not Ready yet
			n.Status.Conditions = []corev1.NodeCondition{{Type: "Ready", Status: "True"}}
		}
		if _, err := cs.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 4 {
		p := podObject(i, config{Nodes: 3, Namespaces: 1})
		p.Status.Phase = corev1.PodRunning
		if _, err := cs.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	ready, running, err := countReady(ctx, cs)
	if err != nil || ready != 2 || running != 4 {
		t.Fatalf("countReady = %d ready, %d running, %v; want 2, 4", ready, running, err)
	}
	wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, _, err = awaitKWOK(wctx, cs, config{Nodes: 3, Pods: 4}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "2/3 nodes Ready") {
		t.Errorf("awaitKWOK with a node not Ready = %v, want a timeout naming 2/3 nodes", err)
	}
	ready, running, err = awaitKWOK(ctx, cs, config{Nodes: 2, Pods: 4}, io.Discard)
	if err != nil || ready != 2 || running != 4 {
		t.Errorf("awaitKWOK = %d, %d, %v; want it to return once 2 nodes and 4 pods are up", ready, running, err)
	}
}

// agent.sh grows one cluster in steps (a quarter, half, all of the full
// seed) and the numbers it reports are for the counts it names, so a step
// must add exactly the difference: the same objects land where the previous
// step put them, as long as the namespace count stays the same.
func TestGrowingTheFillAddsExactlyTheDifference(t *testing.T) {
	cs := fake.NewClientset()
	steps := []config{
		{Nodes: 10, Namespaces: 7, Pods: 20, ConfigMaps: 12, Deployments: 8, HelmReleases: 5, HelmRevisions: 1, KubeletVersion: "v1.37.0", Seed: 1},
		{Nodes: 20, Namespaces: 7, Pods: 40, ConfigMaps: 24, Deployments: 16, HelmReleases: 10, HelmRevisions: 1, KubeletVersion: "v1.37.0", Seed: 1},
	}
	ctx := context.Background()
	for i, cfg := range steps {
		if _, err := run(ctx, cs, cfg, 4, 0, io.Discard); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		nodes, _ := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		pods, _ := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		secrets, _ := cs.CoreV1().Secrets("").List(ctx, metav1.ListOptions{})
		cms, _ := cs.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{})
		deps, _ := cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
		got := []int{len(nodes.Items), len(pods.Items), len(secrets.Items), len(cms.Items), len(deps.Items)}
		want := []int{cfg.Nodes, cfg.Pods, cfg.HelmReleases, cfg.ConfigMaps, cfg.Deployments}
		if !slices.Equal(got, want) {
			t.Errorf("after step %d: nodes, pods, helm secrets, configmaps, deployments = %v, want %v", i, got, want)
		}
	}
}
