# Version skew

Kubernetes components must stay within a fixed distance of the
kube-apiserver, per the upstream
[version skew policy](https://kubernetes.io/releases/version-skew-policy/).
upgradescope checks the components it can see, today and as they would be
once the control plane runs the target.

## What is compared

| Component | Version read from | Policy |
|---|---|---|
| kube-apiserver | `/version`, plus `kube-apiserver` pods in `kube-system` when the control plane runs as pods | HA replicas within 1 minor of each other |
| kubelet | each node's `status.nodeInfo.kubeletVersion` | not newer than the oldest apiserver; at most 3 minors behind the newest (2 for kubelets older than 1.25) |
| kube-controller-manager, kube-scheduler | `kube-system` pod image tags | not newer than the oldest apiserver; at most 1 minor behind the newest |
| kube-proxy | `kube-system` pod image tags | not newer than the oldest apiserver; at most 3 minors behind (2 before 1.25) |

`kubectl` client skew is in the policy but is not checked: client versions
appear only in apiserver audit logs, which upgradescope does not read.

## Findings

| Finding (`key`) | Severity |
|---|---|
| `version-skew/kubelet-post-upgrade`: kubelets that would be more than 3 minors behind once the control plane is at the target | blocker |
| `version-skew/kube-proxy-post-upgrade`: the same for kube-proxy | blocker |
| `version-skew/kube-controller-manager`, `version-skew/kube-scheduler`: newer than the oldest apiserver, whatever the target | blocker |
| `version-skew/kubelet-current`: kubelets too far behind today | warning |
| `version-skew/kubelet-newer-than-apiserver` | warning |
| `version-skew/apiserver-ha-spread` | warning |
| controller-manager or scheduler more than 1 minor behind; kube-proxy too far behind or newer than the apiserver | warning |
| `version-skew/kubelet-unparseable` | info |

Each finding names the nodes or versions, and cites the policy.

## Where the control plane is hidden

On managed control planes (EKS, GKE, AKS) and on distributions that run the
control plane outside pods (k3s), there are no kube-apiserver,
controller-manager or scheduler pods to read. The skew checks then use the
`/version` answer as the apiserver version and simply have no
controller-manager or scheduler to judge: no finding, and no false one.
Kubelets and, where it runs as a pod in `kube-system`, kube-proxy are still
checked. Managed-cluster version strings (`v1.33.1-eks-…`, `-gke.…`, k3s,
RKE2 and OpenShift suffixes) parse
([Managed clusters](../guides/managed-clusters.md)).

The server version is required for a live cluster's verdict: without it no
skew rule has a reference, and the report says so as a required `versions`
gap.
