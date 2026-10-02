package collect

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// platformPod builds a kube-system pod running one container per image.
func platformPod(name string, labels map[string]string, images ...string) *corev1.Pod {
	cs := make([]corev1.Container, len(images))
	for i, im := range images {
		cs[i] = corev1.Container{Name: fmt.Sprintf("c%d", i), Image: im}
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: labels},
		Spec: corev1.PodSpec{Containers: cs}}
}

// The kube-system pods real platforms run, collected and evaluated: every
// platform reads ready, its versions read or, for a vendor image the
// scanner cannot read (OKE's digest-pinned oke-public-kube-proxy), an
// optional partial gap that names it. Only a component whose skew upstream
// would have told and did not — an upstream-named image under a digest or
// "latest", an unread kube-apiserver, kube-controller-manager or
// kube-scheduler pod — is a required gap, so the verdict is unknown.
func TestCollectVersionsPlatformImagesVerdict(t *testing.T) {
	const dg = "sha256:4f8bd1ec8bb2e6a5fcd4b4bbc6e4d5b0fdcb7f0f8a1c8c3f63b5f7f2b1a3c9d1"
	type L = map[string]string
	cases := []struct {
		platform string
		server   string // the apiserver's /version, and every kubelet's
		pods     []*corev1.Pod
		read     []string // component=version read
		partial  bool
		skipped  []string // the components whose unread version is a required gap
	}{
		{"kubeadm", "v1.31.2", []*corev1.Pod{
			platformPod("kube-apiserver-cp1", L{"component": "kube-apiserver", "tier": "control-plane"}, "registry.k8s.io/kube-apiserver:v1.31.2"),
			platformPod("kube-controller-manager-cp1", L{"component": "kube-controller-manager"}, "registry.k8s.io/kube-controller-manager:v1.31.2"),
			platformPod("kube-scheduler-cp1", L{"component": "kube-scheduler"}, "registry.k8s.io/kube-scheduler:v1.31.2"),
			platformPod("etcd-cp1", L{"component": "etcd"}, "registry.k8s.io/etcd:3.5.15-0"),
			platformPod("kube-proxy-x7k2p", L{"k8s-app": "kube-proxy"}, "registry.k8s.io/kube-proxy:v1.31.2"),
			platformPod("coredns-7c65d6cfc9-abcde", L{"k8s-app": "kube-dns"}, "registry.k8s.io/coredns/coredns:v1.11.3"),
		}, []string{"kube-apiserver=v1.31.2", "kube-controller-manager=v1.31.2", "kube-proxy=v1.31.2", "kube-scheduler=v1.31.2"}, false, nil},
		{"kind (tag@digest)", "v1.31.2", []*corev1.Pod{
			platformPod("kube-apiserver-kind-control-plane", L{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.31.2@"+dg),
			platformPod("kube-proxy-2xq9z", L{"k8s-app": "kube-proxy"}, "registry.k8s.io/kube-proxy:v1.31.2"),
			platformPod("kindnet-8hj2k", L{"app": "kindnet", "k8s-app": "kindnet"}, "docker.io/kindest/kindnetd:v20241007-36f62932"),
		}, []string{"kube-apiserver=v1.31.2", "kube-proxy=v1.31.2"}, false, nil},
		{"kind (built from source)", "v1.33.0-alpha.0.1234+0123456789abcd", []*corev1.Pod{
			platformPod("kube-apiserver-kind-control-plane", L{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver-amd64:v1.33.0-alpha.0.1234_0123456789abcd"),
			platformPod("kube-proxy-2xq9z", L{"k8s-app": "kube-proxy"}, "registry.k8s.io/kube-proxy-amd64:v1.33.0-alpha.0.1234_0123456789abcd"),
		}, []string{"kube-apiserver=v1.33.0", "kube-proxy=v1.33.0"}, false, nil},
		{"EKS", "v1.29.0-eks-a5ec690", []*corev1.Pod{
			platformPod("kube-proxy-5d7fh", L{"k8s-app": "kube-proxy"}, "602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/kube-proxy:v1.29.0-minimal-eksbuild.1"),
			platformPod("aws-node-q8w9e", L{"k8s-app": "aws-node"}, "602401143452.dkr.ecr.us-west-2.amazonaws.com/amazon-k8s-cni:v1.18.3-eksbuild.1"),
			platformPod("coredns-787cb67946-abcde", L{"k8s-app": "kube-dns"}, "602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/coredns:v1.11.1-eksbuild.9"),
		}, []string{"kube-proxy=v1.29.0"}, false, nil},
		{"EKS Anywhere", "v1.28.3-eks-1-28-11", []*corev1.Pod{
			platformPod("kube-apiserver-cp1", L{"component": "kube-apiserver"}, "public.ecr.aws/eks-distro/kubernetes/kube-apiserver:v1.28.3-eks-1-28-11"),
			platformPod("kube-proxy-abcde", L{"k8s-app": "kube-proxy"}, "public.ecr.aws/eks-distro/kubernetes/kube-proxy:v1.28.3-eks-1-28-11"),
		}, []string{"kube-apiserver=v1.28.3", "kube-proxy=v1.28.3"}, false, nil},
		{"AKS", "v1.29.4", []*corev1.Pod{
			platformPod("kube-proxy-9pqrs", L{"component": "kube-proxy", "tier": "node"}, "mcr.microsoft.com/oss/kubernetes/kube-proxy:v1.29.4-hotfix.20240527"),
			platformPod("kube-proxy-9pqrt", L{"component": "kube-proxy", "tier": "node"}, "mcr.microsoft.com/oss/v2/kubernetes/kube-proxy:v1.29.4-1"),
			platformPod("konnectivity-agent-abcde", L{"app": "konnectivity-agent", "component": "tunnel"}, "mcr.microsoft.com/oss/kubernetes/apiserver-network-proxy/agent:v0.30.3-hotfix.20240819"),
			platformPod("cloud-node-manager-abcde", L{"component": "cloud-node-manager"}, "mcr.microsoft.com/oss/kubernetes/azure-cloud-node-manager:v1.29.8"),
		}, []string{"kube-proxy=v1.29.4"}, false, nil},
		{"AKS LTS", "v1.28.101-akslts", []*corev1.Pod{
			platformPod("kube-proxy-9pqrs", L{"component": "kube-proxy"}, "mcr.microsoft.com/oss/kubernetes/kube-proxy:v1.28.101-akslts"),
		}, []string{"kube-proxy=v1.28.101"}, false, nil},
		{"GKE (per-arch static pods)", "v1.30.5-gke.1014001", []*corev1.Pod{
			platformPod("kube-proxy-gke-c1-default-pool-1a2b3c4d-wxyz", L{"component": "kube-proxy", "tier": "node"}, "gke.gcr.io/kube-proxy-amd64:v1.30.5-gke.1014001"),
			platformPod("kube-proxy-gke-c1-arm-pool-1a2b3c4d-wxyz", L{"component": "kube-proxy", "tier": "node"}, "gke.gcr.io/kube-proxy-arm64:v1.30.5-gke.1014001"),
			platformPod("konnectivity-agent-abcde", L{"k8s-app": "konnectivity-agent"}, "gke.gcr.io/proxy-agent:v0.30.3-gke.1"),
		}, []string{"kube-proxy=v1.30.5"}, false, nil},
		{"k3s (no component pods)", "v1.31.1+k3s1", []*corev1.Pod{
			platformPod("coredns-7b98449c4-abcde", L{"k8s-app": "kube-dns"}, "rancher/mirrored-coredns-coredns:1.11.3"),
			platformPod("helm-install-traefik-crd-abcde", L{"helmcharts.helm.cattle.io/chart": "traefik-crd"}, "rancher/klipper-helm:v0.9.3-build20241008"),
		}, nil, false, nil},
		{"RKE2", "v1.30.4+rke2r1", []*corev1.Pod{
			platformPod("kube-apiserver-node1", L{"component": "kube-apiserver"}, "index.docker.io/rancher/hardened-kubernetes:v1.30.4-rke2r1-build20240815"),
			platformPod("kube-scheduler-node1", L{"component": "kube-scheduler"}, "index.docker.io/rancher/hardened-kubernetes:v1.30.4-rke2r1-build20240815"),
			platformPod("kube-proxy-node1", L{"component": "kube-proxy"}, "index.docker.io/rancher/hardened-kubernetes:v1.30.4-rke2r1-build20240815"),
			platformPod("cloud-controller-manager-node1", L{"component": "cloud-controller-manager"}, "index.docker.io/rancher/rke2-cloud-provider:v1.29.3-build20240515"),
		}, []string{"kube-apiserver=v1.30.4", "kube-proxy=v1.30.4", "kube-scheduler=v1.30.4"}, false, nil},
		{"OpenShift (no component pods in kube-system)", "v1.31.6", nil, nil, false, nil},
		{"Talos", "v1.31.1", []*corev1.Pod{
			platformPod("kube-apiserver-talos-cp1", L{"k8s-app": "kube-apiserver"}, "ghcr.io/siderolabs/kube-apiserver:v1.31.1"),
			platformPod("kube-proxy-abcde", L{"k8s-app": "kube-proxy"}, "ghcr.io/siderolabs/kube-proxy:v1.31.1"),
		}, []string{"kube-apiserver=v1.31.1", "kube-proxy=v1.31.1"}, false, nil},
		{"kOps (apiserver with a healthcheck sidecar)", "v1.30.2", []*corev1.Pod{
			platformPod("kube-apiserver-i-0123456789abcdef1", L{"k8s-app": "kube-apiserver"}, "registry.k8s.io/kops/kube-apiserver-healthcheck:1.30.0", "registry.k8s.io/kube-apiserver:v1.30.2"),
			platformPod("etcd-manager-main-i-0123456789abcdef0", L{"k8s-app": "etcd-manager-main"}, "registry.k8s.io/etcdadm/etcd-manager-slim:v3.0.20240617"),
		}, []string{"kube-apiserver=v1.30.2"}, false, nil},
		{"LKE, k0s, Alibaba ACK, Gardener, Windows kube-proxy", "v1.30.1", []*corev1.Pod{
			platformPod("kube-proxy-lke", L{"k8s-app": "kube-proxy"}, "linode/kube-proxy:v1.30.1"),
			platformPod("kube-proxy-k0s", L{"k8s-app": "kube-proxy"}, "quay.io/k0sproject/kube-proxy:v1.30.1"),
			platformPod("kube-proxy-worker-abcde", L{"k8s-app": "kube-proxy-worker"}, "registry-cn-hangzhou-vpc.ack.aliyuncs.com/acs/kube-proxy:v1.30.1-aliyun.1"),
			platformPod("kube-proxy-worker-a-v1.30.1-abcde", L{"role": "proxy"}, "registry.k8s.io/kube-proxy:v1.30.1", "europe-docker.pkg.dev/gardener-project/releases/gardener/alpine-conntrack:3.21.2"),
			platformPod("kube-proxy-windows-abcde", L{"k8s-app": "kube-proxy-windows"}, "sigwindowstools/kube-proxy:v1.30.1-calico-hostprocess"),
		}, []string{"kube-proxy=v1.30.1"}, false, nil},
		{"VMware TKG", "v1.28.7+vmware.1", []*corev1.Pod{
			platformPod("kube-apiserver-cp1", L{"component": "kube-apiserver"}, "localhost:5000/vmware.io/kube-apiserver:v1.28.7_vmware.1"),
			platformPod("kube-proxy-abcde", L{"k8s-app": "kube-proxy"}, "projects.registry.vmware.com/tkg/kube-proxy:v1.28.7_vmware.1"),
		}, []string{"kube-apiserver=v1.28.7", "kube-proxy=v1.28.7"}, false, nil},
		{"legacy hyperkube kube-proxy (pre-1.19)", "v1.18.20", []*corev1.Pod{
			platformPod("kube-proxy-abcde", L{"k8s-app": "kube-proxy"}, "k8s.gcr.io/hyperkube:v1.18.20"),
			platformPod("kube-proxy-fghij", L{"k8s-app": "kube-proxy"}, "gcr.io/google-containers/hyperkube-amd64:v1.18.20"),
		}, []string{"kube-proxy=v1.18.20"}, false, nil},
		{"etcd digest-only or latest (not skew-checked)", "v1.31.2", []*corev1.Pod{
			platformPod("etcd-cp1", L{"component": "etcd"}, "registry.k8s.io/etcd@"+dg),
			platformPod("etcd-cp2", L{"component": "etcd"}, "registry.k8s.io/etcd:latest"),
		}, nil, false, nil},
		// OKE runs kube-proxy from a digest-pinned vendor image of another
		// name: its version is not read, disclosed, and optional — the
		// kubelet skew still judges the nodes kube-proxy follows.
		{"OKE (digest-pinned oke-public-kube-proxy)", "v1.30.1", []*corev1.Pod{
			platformPod("kube-proxy-abcde", L{"k8s-app": "kube-proxy"}, "iad.ocir.io/id9y6mi8tcky/oke-public-kube-proxy@sha256:175559244baa3cbee68adfc1357c18871406c7a2d49dd487d2f04e111e7931a0"),
			platformPod("kube-proxy-fghij", L{"k8s-app": "kube-proxy"}, "ap-melbourne-1.ocir.io/axoxdievda5j/oke-public-kube-proxy:v1.30.1"),
		}, nil, true, nil},
		// Genuine unknowns: the upstream image would have told the version.
		{"digest-only upstream kube-proxy", "v1.31.2", []*corev1.Pod{
			platformPod("kube-proxy-x7k2p", L{"k8s-app": "kube-proxy"}, "registry.k8s.io/kube-proxy@"+dg),
		}, nil, true, []string{"kube-proxy"}},
		{"digest-only upstream kube-apiserver", "v1.31.2", []*corev1.Pod{
			platformPod("kube-apiserver-cp1", L{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver@"+dg),
		}, nil, true, []string{"kube-apiserver"}},
		{"kube-scheduler:latest", "v1.31.2", []*corev1.Pod{
			platformPod("kube-scheduler-cp1", nil, "registry.k8s.io/kube-scheduler:latest"),
		}, nil, true, []string{"kube-scheduler"}},
		{"a labelled kube-controller-manager running a vendor image", "v1.31.2", []*corev1.Pod{
			platformPod("kube-controller-manager-cp1", L{"component": "kube-controller-manager"}, "registry.example.com/platform/kcm:2.1"),
		}, nil, true, []string{"kube-controller-manager"}},
		// scheduler-plugins' kube-scheduler is tagged with its own
		// version (v0.29.7), not Kubernetes': not read as the upstream
		// scheduler, so no bogus skew finding; as the cluster's scheduler
		// its skew is unjudged.
		{"scheduler-plugins as the cluster's kube-scheduler", "v1.31.2", []*corev1.Pod{
			platformPod("kube-scheduler-cp1", L{"component": "kube-scheduler"}, "registry.k8s.io/scheduler-plugins/kube-scheduler:v0.29.7"),
		}, nil, true, []string{"kube-scheduler"}},
	}

	k := kb.KB{Version: "test", Skew: kb.DefaultSkewPolicy(), MaxKnownK8s: inventory.Version{Major: 1, Minor: 36}}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			objs := []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid")}},
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: tc.server}}},
			}
			for _, p := range tc.pods {
				objs = append(objs, p)
			}
			cs := kubefake.NewClientset(objs...)
			disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
			disc.FakedServerVersion = &version.Info{GitVersion: tc.server}

			inv := inventory.Inventory{Source: inventory.SourceCluster, Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
			for _, c := range []inventory.Capability{inventory.CapAPIUsage, inventory.CapDeprecatedCalls, inventory.CapHelm, inventory.CapAddOns, inventory.CapCRDs} {
				inv.Capabilities[c] = inventory.CapabilityStatus{Available: true}
			}
			runSteps(context.Background(), &inv, []step{{cap: inventory.CapVersions, run: func(ctx context.Context, inv *inventory.Inventory) error {
				return collectVersions(ctx, disc, cs, "team", inv)
			}}})

			var read []string
			for _, cv := range inv.ControlPlane {
				read = append(read, cv.Component+"="+cv.Version)
			}
			st := inv.Capabilities[inventory.CapVersions]
			if !reflect.DeepEqual(read, tc.read) || !st.Available || st.Partial != tc.partial || !reflect.DeepEqual(st.Skipped, tc.skipped) {
				t.Fatalf("read %q, versions %+v; want read %q, partial %v, skipped %q", read, st, tc.read, tc.partial, tc.skipped)
			}
			if tc.partial && !strings.Contains(st.Reason, "kube-system/") {
				t.Errorf("reason %q names no pod", st.Reason)
			}

			target, err := inventory.ParseVersion(tc.server)
			if err != nil {
				t.Fatal(err)
			}
			target.Minor++
			r := engine.Evaluate(inv, k, target, now)
			want := engine.VerdictReady
			if len(tc.skipped) > 0 {
				want = engine.VerdictUnknown
			}
			if r.Verdict != want {
				t.Errorf("verdict %s, want %s; findings %+v, gaps %+v", r.Verdict, want, r.Findings, r.NotAssessed)
			}
		})
	}
}
