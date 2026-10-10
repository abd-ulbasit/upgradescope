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
| kube-proxy | `kube-system` pod image tags, one per node (`spec.nodeName`) | not newer than the oldest apiserver; at most 3 minors behind (2 before 1.25); within 3 minors (2 before 1.25), older or newer, of the kubelet on the same node |

A component pod is one labelled `component=<name>` or `k8s-app=<name>`
(the kube-proxy DaemonSet, kOps and Talos static pods), or one named
`<name>-…` that runs the component's image. Its version is
the tag of the container whose image is named `<name>` or, for
per-architecture images such as GKE's `gke.gcr.io/kube-proxy-amd64` and
older kubeadm's `kube-scheduler-amd64`, `<name>-<arch>`, or RKE2's
`rancher/hardened-kubernetes`, which runs every component and is tagged
with the Kubernetes version; a build suffix (`-gke.1000`, `-eksbuild.1`,
`-rke2r1-build…`, VMware TKG's `_vmware.1`) is dropped. scheduler-plugins'
`kube-scheduler` (`registry.k8s.io/scheduler-plugins/kube-scheduler:v0.31.8`),
which its single-scheduler install swaps into the kube-scheduler static
pod, is tagged with that project's version, whose minor is the Kubernetes
minor it is compiled with: `v0.31.8` reads as `v1.31.8`, and a three-digit
patch (`v0.18.800`, plugin changes only) as `v1.18.0`. A component pod
whose version cannot be read — a digest-only image, a tag that is not a
version (`latest`), or a labelled pod that runs a vendor image (one not
named like the component) — is not skipped silently: the `versions`
capability is reported partial, naming the components and the first such
pod with its image (the first of a component whose gap is required, when
there is one), because its skew was not evaluated.

The gap is *required*, so the verdict is `unknown`, never `ready`, when
upstream would have told the version and did not: a component image named
like the component whose tag is not a version, or any unread
kube-apiserver, kube-controller-manager or kube-scheduler pod. That
component may be the one past the policy. `--allow-incomplete` lets the
gate (the exit code, and the JUnit `not-assessed` case) pass on the
findings alone; the verdict still reads `unknown`. Pin the component's
image to a version tag to have its skew judged.

A kube-proxy pod running a vendor image of another name is an *optional*
gap: disclosed, verdict unaffected. Platforms ship kube-proxy that way
(Oracle OKE runs `<region>.ocir.io/…/oke-public-kube-proxy@sha256:…`,
pinned by digest) and manage and upgrade it themselves, and no upstream
image would have told its version. Its skew against the target is then
the platform's to keep, not judged here.

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
| `version-skew/kube-proxy-kubelet/<node>`: a kube-proxy more than 3 minors (2 before 1.25) older or newer than the kubelet on the same node; names the node and cites the [policy](https://kubernetes.io/releases/version-skew-policy/#kube-proxy) | warning |
| `version-skew/apiserver-ha-spread` | warning |
| `version-skew/kube-controller-manager-behind`, `version-skew/kube-scheduler-behind` (more than 1 minor behind the newest apiserver), `version-skew/kube-proxy-behind` | warning |
| `version-skew/kubelet-unparseable` | info |
| `version-skew/upgrade-path`: the target is more than one minor ahead of the oldest apiserver; names each minor the control plane passes through | info |

One component can be both newer and behind at once (HA replicas
mid-upgrade), so each direction has its own key.

The kube-proxy and kubelet pairing is the only rule between two components
on one node, and it does not depend on the target: neither moves when the
control plane does. It adds clarity rather than detection, since a pair
more than 3 minors apart already breaks a rule against the apiserver; the
finding names the node, which makes a mixed node pool easier to fix. A
kube-proxy pod that names no node (not scheduled yet, or from an agent
that predates the field) or whose node is not listed is not paired, and
gives no finding.

## When no node is listed

With an empty Node list there is no kubelet to judge and no node runtime
to check, and a cluster does not read as clean on them: the `versions`
capability is reported partial, with the reason `no nodes listed: kubelet
skew and node runtimes not assessed` and `nodes` in `skipped`. The gap is
*required* on a live cluster, so the verdict is `unknown`, exactly as when
the Node list is forbidden: a kubelet past the policy would be a blocker.

## A target that is not an upgrade

The control plane is upgraded one minor at a time, and every check judges a
newer minor. On a live cluster, a `--target` at or below the minor the
oldest kube-apiserver runs (a downgrade or the same minor) is reported as a
required `target` gap: the verdict is `unknown` and `scan` exits 2, even with
`--allow-incomplete`; only `--fail-on never`, which always exits 0, passes it.
The table and the JSON report show the server version the target was judged
against (`serverVersion`).

A typo such as `1.4` for `1.40` (what YAML makes of an unquoted
`target: 1.40`) is below the oldest minor the knowledge base covers, 1.16, and
is refused before anything is scanned, live or with `--files`: `scan` exits 1,
the agent and `serve` refuse to start, a `spec.targets` entry is skipped with
a note, and the server's gate answers 400. The message says to quote the
version. A target between 1.16 and the cluster's own minor is a downgrade, the
gap above.

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
