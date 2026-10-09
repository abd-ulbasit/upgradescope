# Managed clusters and distributions

Point `scan` or the agent at any conformant cluster; there is nothing to
configure per provider. What differs is what the provider lets a client
see.

## What changes on a managed control plane

- **`/metrics` is often forbidden.** EKS, GKE and AKS commonly deny the
  apiserver's `/metrics` endpoint whatever your RBAC says. The
  `deprecated-calls` capability is then reported as not assessed, with the
  reason (`apiserver /metrics forbidden (managed control planes often block
  this; …)`). You lose the request signal (deprecated APIs requested by
  clients that store nothing); stored objects and manifests are still
  checked, and the verdict does not depend on it
  ([Deprecated-API detection](../concepts/api-usage-detection.md)).
- **No control-plane pods.** The provider runs kube-apiserver,
  controller-manager and scheduler outside the cluster, so there are no
  pods to read. Their skew rules have nothing to compare and produce no
  finding (and no false one); the apiserver version comes from `/version`.
  Kubelets are still checked against it, and so is kube-proxy wherever it
  runs as pods in `kube-system`, including GKE's per-architecture
  `gke.gcr.io/kube-proxy-amd64` image. A kube-proxy pod whose version
  cannot be read from its image makes the `versions` capability partial,
  naming the pod, rather than passing silently. On the `kube-proxy` image
  under a digest or `latest`, that gap is required and the verdict is
  `unknown` (pin a version tag, or gate with `--allow-incomplete`); on a
  vendor image of another name (OKE's) it is optional and the verdict is
  unaffected ([Version skew](../concepts/version-skew.md)).
- **Provider version strings parse**: `v1.33.1-eks-…`, `v1.33.1-gke.…`,
  and the k3s, RKE2 and OpenShift suffixes (tested with observed git
  versions, `TestParseVersionObservedGitVersions`).
- **Provider builds of add-ons are not judged as upstream.** Images under
  `gke.gcr.io/`, `gcr.io/gke-release/` and `mcr.microsoft.com/` follow the
  provider's support policy, so upstream release lines are not applied to
  them ([Add-on registry](../concepts/addon-registry.md)).
- **Support dates and what waiting costs.** On EKS, GKE and AKS the scanner
  dates the day your minor enters extended support and, where the provider
  publishes a price, the annual list-price delta
  ([Managed-provider support](../concepts/support-lifecycle.md)).

Everything else, objects, add-ons from images and Helm releases, the score,
the `ClusterReadiness` status and server pushes, works the same way.

## Providers and distributions

| Platform | Notes |
|---|---|
| **EKS** | Expect `deprecated-calls` not assessed. Amazon's own [upgrade insights](https://docs.aws.amazon.com/eks/latest/userguide/cluster-insights.html) also flag deprecated API use; see [Comparison](../comparison.md). |
| **GKE** | Expect `deprecated-calls` not assessed. GKE pauses automatic minor upgrades when it sees deprecated API calls ([Feature and API deprecations](https://docs.cloud.google.com/kubernetes-engine/docs/deprecations)). GKE's own Calico and Dataplane V2 Cilium images are provider builds and are not judged against upstream EOL. |
| **AKS** | Expect `deprecated-calls` not assessed. AKS can stop a minor upgrade on recent deprecated API use ([docs](https://learn.microsoft.com/en-us/azure/aks/stop-cluster-upgrade-api-breaking-changes)). The registry has an entry for the application routing add-on's NGINX build (`aks-app-routing-nginx`), separate from upstream Ingress NGINX. |
| **OKE** | kube-proxy runs from Oracle's digest-pinned vendor image (`<region>.ocir.io/…/oke-public-kube-proxy@sha256:…`), so its version is not read: kube-proxy skew is not evaluated, an optional `versions (partial)` gap names it, and the verdict is unaffected. kube-proxy there is managed and upgraded by the platform. |
| **k3s** | The control plane runs inside the k3s binary, not as pods: no controller-manager or scheduler skew. |
| **RKE2** | The control plane runs as static pods in `kube-system`, so its skew is checked. RKE2's bundled ingress controller has its own registry entry (`rke2-ingress-nginx`, by its `-hardenedN` image tags): SUSE builds for community users ended in March 2026, so it is a warning stating that support continues until 2027-11-30 only with a SUSE Rancher Prime LTS subscription. RKE1's build of the same image (`-rancherN` tags) is Ingress NGINX, end of life. |
| **OpenShift** | The chart pins user and group IDs 65532, which the `restricted-v2` SCC rejects. Unset them so the SCC assigns its own: `--set-json 'agent.podSecurityContext={"runAsUser":null,"runAsGroup":null}'` (and `server.podSecurityContext` with `fsGroup` too, when the server runs). `runAsNonRoot` and the `RuntimeDefault` seccomp profile stay. |

The provider rows above describe what the code does with what each
platform exposes; they are covered by unit tests on recorded inputs, not by
end-to-end runs on each provider (that audit is open, AO-06 in the
[claims ledger](../claims.md#not-yet-audited)).

## Permissions

The agent's ClusterRole is the same everywhere
([Security model and RBAC](../operations/security-model-and-rbac.md)). On
platforms where your own identity cannot read some resources, a `scan` from
your laptop reports each unreadable part as not assessed with the
forbidden error, and the verdict says whether that gap matters.
