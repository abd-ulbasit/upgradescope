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

A component pod is one labelled `component=<name>` (or `k8s-app=kube-proxy`),
or one named `<name>-…` that runs the component's image. Its version is
the tag of the container whose image is named `<name>` or, for
per-architecture images such as GKE's `gke.gcr.io/kube-proxy-amd64` and
older kubeadm's `kube-scheduler-amd64`, `<name>-<arch>`; a build suffix
(`-gke.1000`, `-eksbuild.1`) is dropped. A component pod whose version
cannot be read — a digest-only image, a tag that is not a version
(`latest`), or a labelled pod that runs no image of the component's name
(a vendor image named otherwise) — is not skipped silently: the
`versions` capability is reported partial, naming the components and the
first such pod with its image, because its skew was not evaluated.

`kubectl` client skew is in the policy but is not checked: client versions
appear only in apiserver audit logs, which upgradescope does not read.

## Findings

| Finding (`key`) | Severity |
|---|---|
| `version-skew/kubelet-post-upgrade`: kubelets that would be more than 3 minors behind once the control plane is at the target | blocker |
| `version-skew/kube-proxy-post-upgrade`: the same for kube-proxy | blocker |
| `version-skew/kube-controller-manager-newer`, `version-skew/kube-scheduler-newer`: newer than the oldest apiserver, whatever the target | blocker |
| `version-skew/kubelet-current`: kubelets too far behind today | warning |
| `version-skew/kubelet-newer-than-apiserver` | warning |
| `version-skew/kube-proxy-newer`: kube-proxy newer than the oldest apiserver | warning |
| `version-skew/apiserver-ha-spread` | warning |
| `version-skew/kube-controller-manager-behind`, `version-skew/kube-scheduler-behind` (more than 1 minor behind the newest apiserver), `version-skew/kube-proxy-behind` | warning |
| `version-skew/kubelet-unparseable` | info |
| `version-skew/upgrade-path`: the target is more than one minor ahead of the oldest apiserver; names each minor the control plane passes through | info |

One component can be both newer and behind at once (HA replicas
mid-upgrade), so each direction has its own key.

## A target that is not an upgrade

The control plane is upgraded one minor at a time, and every check judges a
newer minor. On a live cluster, a `--target` at or below the minor the
oldest kube-apiserver runs (a downgrade, the same minor, or a typo such as
`1.4` for `1.40`) is reported as a required `target` gap: the verdict is
`unknown` and `scan` exits 2, even with `--allow-incomplete`. The table and
the JSON report show the server version the target was judged against
(`serverVersion`).

Each finding names the nodes or versions, and cites the policy.

## Where the control plane is hidden

On managed control planes (EKS, GKE, AKS) and on distributions that run the
control plane outside pods (k3s), there are no kube-apiserver,
controller-manager or scheduler pods to read. The skew checks then use the
`/version` answer as the apiserver version and simply have no
controller-manager or scheduler to judge: no finding, and no false one.
Kubelets and, where it runs as a pod in `kube-system`, kube-proxy are still
checked (GKE's `kube-proxy-amd64` image included). Managed-cluster version strings (`v1.33.1-eks-…`, `-gke.…`, k3s,
RKE2 and OpenShift suffixes) parse
([Managed clusters](../guides/managed-clusters.md)).

The server version is required for a live cluster's verdict: without it no
skew rule has a reference, and the report says so as a required `versions`
gap.
