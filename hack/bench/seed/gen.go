package main

import (
	"fmt"
	"math/rand/v2"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// KWOK manages a node only when it carries this annotation (the controller
// is deployed with manageNodesWithAnnotationSelector set to it, see
// hack/bench/agent.sh), so the lab's one real node is never touched.
const (
	kwokNodeAnnotation = "kwok.x-k8s.io/node"
	kwokNodeValue      = "fake"
	// benchLabel marks everything this seeder creates.
	benchLabel = "upgradescope.basit.engineer/bench"
)

// config is what to seed. Zero counts seed none of that kind.
type config struct {
	Nodes, Namespaces, Pods, ConfigMaps, Deployments int
	HelmReleases, HelmRevisions                      int
	// The opt-in GitOps fill (gitops.go, #233): zero seeds none.
	ArgoApps, FluxHelmReleases int
	KubeletVersion             string
	Seed                       uint64
}

func nsName(i int) string   { return fmt.Sprintf("bench-ns-%03d", i) }
func nodeName(i int) string { return fmt.Sprintf("kwok-node-%05d", i) }

func benchLabels(extra map[string]string) map[string]string {
	l := map[string]string{benchLabel: "true"}
	for k, v := range extra {
		l[k] = v
	}
	return l
}

func namespaceObject(i int) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   nsName(i),
		Labels: benchLabels(map[string]string{"team": fmt.Sprintf("team-%02d", i%12)}),
	}}
}

// nodeObject is a KWOK fake node that reports Ready with a kubelet version:
// status.nodeInfo is kept by KWOK's node-initialize stage when set. The
// taint keeps the real scheduler from placing real pods on it.
func nodeObject(i int, kubeletVersion string) *corev1.Node {
	rl := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("32"),
		corev1.ResourceMemory: resource.MustParse("256Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName(i),
			Annotations: map[string]string{kwokNodeAnnotation: kwokNodeValue},
			Labels: benchLabels(map[string]string{
				"type":                             "kwok",
				"kubernetes.io/hostname":           nodeName(i),
				"kubernetes.io/os":                 "linux",
				"kubernetes.io/arch":               "amd64",
				"topology.kubernetes.io/zone":      fmt.Sprintf("zone-%c", 'a'+rune(i%3)),
				"node.kubernetes.io/instance-type": "m5.8xlarge",
			}),
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{Key: "kwok.x-k8s.io/node", Value: "fake", Effect: corev1.TaintEffectNoSchedule}},
		},
		Status: corev1.NodeStatus{
			Capacity:    rl,
			Allocatable: rl,
			NodeInfo: corev1.NodeSystemInfo{
				Architecture:            "amd64",
				OperatingSystem:         "linux",
				OSImage:                 "Ubuntu 24.04.2 LTS",
				KernelVersion:           "6.8.0-60-generic",
				ContainerRuntimeVersion: "containerd://2.1.3",
				KubeletVersion:          kubeletVersion,
				KubeProxyVersion:        kubeletVersion,
			},
		},
	}
}

// images are what the pods run: common application images, plus a few
// recognised add-ons so the add-on matcher has work to do.
var images = []string{
	"docker.io/library/nginx:1.27.3", "docker.io/library/redis:7.4.1", "docker.io/library/postgres:16.4",
	"ghcr.io/example/api:v2.31.0", "ghcr.io/example/worker:v2.31.0", "quay.io/example/frontend:1.18.4",
	"registry.k8s.io/pause:3.10", "docker.io/library/busybox:1.37.0", "gcr.io/example/batch:2024.10.2",
	"docker.io/envoyproxy/envoy:v1.32.1", "docker.io/library/memcached:1.6.32", "ghcr.io/example/cron:v0.9.4",
}

// addOnImages are placed on every addOnEvery-th pod.
var addOnImages = []struct{ image, app string }{
	{"registry.k8s.io/ingress-nginx/controller:v1.11.2", "ingress-nginx"},
	{"quay.io/jetstack/cert-manager-controller:v1.16.1", "cert-manager"},
	{"registry.k8s.io/coredns/coredns:v1.11.3", "kube-dns"},
	{"docker.io/grafana/grafana:11.3.0", "grafana"},
}

const addOnEvery = 500

func podObject(i int, cfg config) *corev1.Pod {
	image := images[i%len(images)]
	labels := map[string]string{"app": fmt.Sprintf("app-%d", i%400), "pod-index": fmt.Sprint(i)}
	if i%addOnEvery == 0 {
		a := addOnImages[(i/addOnEvery)%len(addOnImages)]
		image = a.image
		labels = map[string]string{"app.kubernetes.io/name": a.app, "app.kubernetes.io/instance": a.app}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("pod-%05d", i),
			Namespace: nsName(i % max(cfg.Namespaces, 1)),
			Labels:    benchLabels(labels),
		},
		Spec: corev1.PodSpec{
			// Placed on a node directly: no scheduler work, KWOK marks it Running.
			NodeName:    nodeName(i % max(cfg.Nodes, 1)),
			Tolerations: []corev1.Toleration{{Key: "kwok.x-k8s.io/node", Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:  "main",
				Image: image,
				Env:   []corev1.EnvVar{{Name: "POD_INDEX", Value: fmt.Sprint(i)}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
			}},
		},
	}
}

func configMapObject(i int, cfg config) *corev1.ConfigMap {
	rng := rand.New(rand.NewPCG(cfg.Seed, uint64(i)))
	data := map[string]string{}
	for k := range 4 {
		data[fmt.Sprintf("setting-%d.properties", k)] = fmt.Sprintf("endpoint=https://svc-%d.internal:%d\ntimeout=%ds\nretries=%d\ntoken-hint=%016x\n",
			rng.IntN(1000), 8000+rng.IntN(1000), 1+rng.IntN(60), rng.IntN(8), rng.Uint64())
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("config-%05d", i), Namespace: nsName(i % max(cfg.Namespaces, 1)), Labels: benchLabels(nil)},
		Data:       data,
	}
}

// deploymentObject is a bare Deployment at zero replicas: the object costs
// etcd and list bytes, and the deployment controller creates no pods.
func deploymentObject(i int, cfg config) *appsv1.Deployment {
	app := fmt.Sprintf("deploy-%05d", i)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: nsName(i % max(cfg.Namespaces, 1)), Labels: benchLabels(map[string]string{"app": app})},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](0),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "main", Image: images[i%len(images)],
				}}},
			},
		},
	}
}
