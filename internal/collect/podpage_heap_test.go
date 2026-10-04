package collect

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// productionPod is a pod the size of a production cluster's, not a KWOK
// one's: two containers with probes, resources, environment and mounts, a
// sidecar-injected init container, the labels and annotations a chart and
// a mesh add, every field manager's managedFields, and a status with
// conditions and container statuses.
func productionPod(i, envVars int) corev1.Pod {
	ns := fmt.Sprintf("team-%d", i%40)
	name := fmt.Sprintf("app-%d-7d9c8b6f5-%05d", i%300, i)
	var env []corev1.EnvVar
	for e := range envVars {
		env = append(env, corev1.EnvVar{Name: fmt.Sprintf("APP_SETTING_%02d", e), Value: fmt.Sprintf("value-%d-%d-%016x", i, e, uint64(i)*2654435761+uint64(e))})
	}
	container := func(cname, image string) corev1.Container {
		return corev1.Container{
			Name: cname, Image: image, Env: env,
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}, {Name: "metrics", ContainerPort: 9090, Protocol: corev1.ProtocolTCP}},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "config", MountPath: "/etc/app"}, {Name: "kube-api-access-x7k2p", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
				{Name: "tmp", MountPath: "/tmp"},
			},
			LivenessProbe:          &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http")}}, PeriodSeconds: 10},
			ReadinessProbe:         &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromString("http")}}, PeriodSeconds: 5},
			TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			ImagePullPolicy: corev1.PullIfNotPresent,
		}
	}
	fields := func(manager, op string, n int) metav1.ManagedFieldsEntry {
		var raw bytes.Buffer
		raw.WriteString(`{"f:metadata":{"f:labels":{`)
		for k := range n {
			if k > 0 {
				raw.WriteByte(',')
			}
			fmt.Fprintf(&raw, `".":{},"f:label-%d":{}`, k)
		}
		raw.WriteString(`}},"f:spec":{"f:containers":{"k:{\"name\":\"app\"}":{".":{},"f:env":{".":{},"k:{\"name\":\"APP_SETTING_00\"}":{".":{},"f:name":{},"f:value":{}}},"f:image":{},"f:imagePullPolicy":{},"f:livenessProbe":{".":{},"f:httpGet":{".":{},"f:path":{},"f:port":{}}},"f:ports":{},"f:resources":{".":{},"f:limits":{},"f:requests":{}}}}}}`)
		return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationType(op), APIVersion: "v1", FieldsType: "FieldsV1",
			Time: &metav1.Time{Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}, FieldsV1: &metav1.FieldsV1{Raw: raw.Bytes()}}
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, GenerateName: name[:len(name)-5], Namespace: ns, UID: types.UID(fmt.Sprintf("0d3c6c84-6f43-4a1b-9a5e-%012d", i)),
			ResourceVersion: strconv.Itoa(1000000 + i),
			Labels: map[string]string{
				"app.kubernetes.io/name": fmt.Sprintf("app-%d", i%300), "app.kubernetes.io/instance": fmt.Sprintf("app-%d", i%300),
				"app.kubernetes.io/version": "1.27.3", "app.kubernetes.io/part-of": "platform", "app.kubernetes.io/managed-by": "Helm",
				"helm.sh/chart": "app-4.2.1", "pod-template-hash": "7d9c8b6f5", "security.istio.io/tlsMode": "istio",
				"service.istio.io/canonical-name": fmt.Sprintf("app-%d", i%300), "service.istio.io/canonical-revision": "1.27.3",
			},
			Annotations: map[string]string{
				"kubectl.kubernetes.io/default-container": "app", "prometheus.io/scrape": "true", "prometheus.io/port": "9090",
				"checksum/config":                   fmt.Sprintf("%064x", uint64(i)*11400714819323198485),
				"sidecar.istio.io/status":           `{"initContainers":["istio-init"],"containers":["istio-proxy"],"volumes":["workload-socket","credential-socket","workload-certs","istio-envoy","istio-data","istio-podinfo","istio-token","istiod-ca-cert"],"imagePullSecrets":null,"revision":"default"}`,
				"istio.io/rev":                      "default",
				"kubectl.kubernetes.io/restartedAt": "2026-09-14T08:12:44Z",
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: name[:len(name)-6], UID: "8f1d2c3b-4a5e-6f70-8192-a3b4c5d6e7f8"}},
			ManagedFields:   []metav1.ManagedFieldsEntry{fields("kube-controller-manager", "Update", 10), fields("kubelet", "Update", 2), fields("istio-sidecar-injector", "Update", 6)},
		},
		Spec: corev1.PodSpec{
			InitContainers:     []corev1.Container{container("istio-init", "docker.io/istio/proxyv2:1.27.3")},
			Containers:         []corev1.Container{container("app", "ghcr.io/example/app:1.27.3"), container("istio-proxy", "docker.io/istio/proxyv2:1.27.3")},
			Volumes:            []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}}}}, {Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			NodeName:           fmt.Sprintf("ip-10-0-%d-%d.eu-west-1.compute.internal", i%250, i%200),
			ServiceAccountName: "app", SchedulerName: "default-scheduler", RestartPolicy: corev1.RestartPolicyAlways,
			Tolerations: []corev1.Toleration{{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}, {Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, HostIP: "10.0.1.17", PodIP: fmt.Sprintf("10.244.%d.%d", i%250, i%200), QOSClass: corev1.PodQOSBurstable,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}, {Type: corev1.ContainersReady, Status: corev1.ConditionTrue}, {Type: corev1.PodInitialized, Status: corev1.ConditionTrue}, {Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true, Image: "ghcr.io/example/app:1.27.3", ImageID: "ghcr.io/example/app@sha256:" + fmt.Sprintf("%064x", i), ContainerID: "containerd://" + fmt.Sprintf("%064x", i+1)},
				{Name: "istio-proxy", Ready: true, Image: "docker.io/istio/proxyv2:1.27.3", ImageID: "docker.io/istio/proxyv2@sha256:" + fmt.Sprintf("%064x", 7), ContainerID: "containerd://" + fmt.Sprintf("%064x", i+2)},
			},
		},
	}
}

// smallPod is a pod of about 140 bytes in protobuf: a batch job's, with no
// probes, environment or status to speak of.
func smallPod(i int) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("job-%05d", i), Namespace: "batch"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "job", Image: "ghcr.io/example/job:1.0.0"}}},
	}
}

// podPageServer serves pods pods, pod(i) the i-th, in protobuf, as an
// apiserver does, in the pages the collector asks for (listPageSize, then
// what pageLimit gives for the page before), and returns the encoded size
// of the largest pod and the page limits it served. The pages are encoded
// before it starts, so serving them adds next to nothing to the heap the
// client is measured by; a request for another page fails.
func podPageServer(t testing.TB, pods int, pod func(i int) corev1.Pod) (*httptest.Server, int, []int64) {
	t.Helper()
	enc := protobuf.NewSerializer(kubescheme.Scheme, kubescheme.Scheme)
	type page struct {
		limit int64
		body  []byte
	}
	pages := map[int]page{} // first pod's index → the page
	var limits []int64
	most := 0
	for start, limit := 0, int64(listPageSize); start < pods; {
		end := min(pods, start+int(limit))
		list := &corev1.PodList{}
		if end < pods {
			list.Continue = strconv.Itoa(end)
		}
		for i := start; i < end; i++ {
			list.Items = append(list.Items, pod(i))
		}
		var b bytes.Buffer
		if err := enc.Encode(list, &b); err != nil {
			t.Fatal(err)
		}
		pages[start] = page{limit, b.Bytes()}
		limits = append(limits, limit)
		largest := largestPod(list.Items)
		most = max(most, largest)
		start, limit = end, pageLimit(largest, podPageSize)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("continue"))
		p, ok := pages[start]
		if r.URL.Path != "/api/v1/pods" { // IngressClasses: not served, which is no failure
			http.NotFound(w, r)
			return
		}
		if !ok || r.URL.Query().Get("limit") != strconv.FormatInt(p.limit, 10) {
			http.Error(w, fmt.Sprintf("no page at %d of limit %s", start, r.URL.Query().Get("limit")), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.kubernetes.protobuf")
		w.Write(p.body)
	}))
	t.Cleanup(srv.Close)
	return srv, most, limits
}

// largestPod is the encoded size of the largest of pods.
func largestPod(pods []corev1.Pod) int {
	n := 0
	for i := range pods {
		n = max(n, pods[i].Size())
	}
	return n
}

// TestPodPagePeakHeapIsBounded bounds what one page of the pod list holds
// (#228): client-go reads a page's response whole and decodes it whole, so
// the page size sets the heap of the pod pass, and a page holds at most
// podPageSize pods of any size. Its worst case is a page of small pods
// followed by large ones: the small ones size the next page at the most,
// and the large ones fill it. The cases: production pods (productionPod:
// about 8 KiB each in protobuf, managedFields included, about three times
// the scale lab's KWOK pods); pods with ten times the environment (39 to
// 41 KiB), which stay at listPageSize, the page every list had before
// #228; and 500 small pods (137 bytes) followed by those large ones, the
// worst case: a whole page of podPageSize large pods, 39.4 MiB encoded.
// Each is listed through the agent's clients, the add-ons keeping only
// images and labels, and the live heap above the baseline must stay under
// 128 MiB, half the chart's 256Mi limit. The margin is thin on purpose: the
// worst case measured 124.5 to 125.5 MiB (Apple M1 Pro, 4 October 2026;
// the other cases about 35 and 64 MiB), 2.5 to 3.5 MiB under the limit, so
// a change that makes a page's decoding about 3% larger fails here. Smaller
// large pods would give it room only by testing less than the 1,000 a page
// may hold of them; docs/claims.md (PF-02) states the same margin. A heap
// figure, run by hack/test-heap.sh (UPGRADESCOPE_HEAP=1) only. Under the
// race detector it lists fewer.
func TestPodPagePeakHeapIsBounded(t *testing.T) {
	if testing.Short() || !heapRun {
		t.Skip("lists 6,000 production-sized pods; a heap figure, run by hack/test-heap.sh (UPGRADESCOPE_HEAP=1)")
	}
	large := func(i int) corev1.Pod { return productionPod(i, 240) }
	for _, tc := range []struct {
		name string
		pod  func(i int) corev1.Pod
		pods int
	}{
		{"production pods", func(i int) corev1.Pod { return productionPod(i, 24) }, 6001},
		{"pods with large environments", large, 4 * listPageSize},
		{"small pods, then a page of pods with large environments", func(i int) corev1.Pod {
			if i < listPageSize {
				return smallPod(i)
			}
			return large(i)
		}, listPageSize + podPageSize + listPageSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := tc.pods
			if raceEnabled {
				pods = min(pods, listPageSize+podPageSize)
			}
			srv, size, limits := podPageServer(t, pods, tc.pod)
			c, err := NewClients(&rest.Config{Host: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			var inv inventory.Inventory
			var cerr error
			var peak uint64
			for attempt := 1; ; attempt++ {
				inv = inventory.Inventory{}
				peak = peakLiveHeap(func() { cerr = collectAddOns(context.Background(), c.Kube, testRegistry(), &inv) })
				t.Logf("%d pods, the largest %d B in protobuf, pages of %v: live heap peak above baseline %.1f MiB (attempt %d)", pods, size, limits, float64(peak)/(1<<20), attempt)
				if peak <= 128<<20 || attempt == maxManifestAttempts {
					break
				}
			}
			if cerr != nil {
				t.Fatal(cerr)
			}
			if len(inv.AddOns) == 0 {
				t.Fatalf("no add-on detected from %d pods: the pods were not read", pods)
			}
			if peak > 128<<20 {
				t.Errorf("live heap peak %.1f MiB, want ≤ 128 MiB", float64(peak)/(1<<20))
			}
		})
	}
}

// A page after the first holds as many objects as fit wholePageBytes at the
// size of the largest object of the page before, within listPageSize and
// the resource's most (#228).
func TestPageLimit(t *testing.T) {
	for _, tc := range []struct {
		largest int
		most    int64
		want    int64
	}{
		{0, podPageSize, listPageSize},         // an empty page says nothing
		{150, podPageSize, podPageSize},        // small pods: past the most
		{3 << 10, podPageSize, podPageSize},    // KWOK-sized pods (3 KiB): 2,730 fit, past the most
		{10 << 10, podPageSize, 819},           // a production pod of 10 KiB
		{8388, nodePageSize, 1000},             // 8 MiB / 8,388 B is 1,000.07
		{8389, nodePageSize, 999},              // a byte more: 999.95, rounded down
		{40 << 10, podPageSize, listPageSize},  // 40 KiB pods: never under listPageSize
		{20 << 10, nodePageSize, listPageSize}, // production nodes (20 KiB, the images they hold)
		{1 << 20, podPageSize, listPageSize},   // one pod of 1 MiB: listPageSize, as before
		{(8 << 20) / 600, nodePageSize, 600},   // in between
	} {
		if got := pageLimit(tc.largest, tc.most); got != tc.want {
			t.Errorf("pageLimit(%d, %d) = %d, want %d", tc.largest, tc.most, got, tc.want)
		}
	}
}
