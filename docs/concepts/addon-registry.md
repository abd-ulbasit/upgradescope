# Add-on registry

The add-on registry is upgradescope's public dataset of add-on end-of-life
dates, release lines and Kubernetes compatibility: one YAML file per add-on
in [`registry/data/`](https://github.com/abd-ulbasit/upgradescope/tree/main/registry/data).
It is importable as a Go package on its own (`registry`), compiled into the
binary, and browsable from a running server at `GET /api/v1/registry` and in
the dashboard.

## How an add-on is found

- **Container images.** Every pod's images are matched by repository path,
  as a suffix on whole path segments, so mirrors and pull-through caches
  match too; the version is read from the tag. Images of provider builds
  (`gke.gcr.io/`, `gcr.io/gke-release/`, `mcr.microsoft.com/`) follow the
  provider's support policy and only match entries written for them.
- **Helm releases.** A release whose chart name is in an entry's matchers is
  that add-on, at the release's `appVersion` (never the chart version).
  In the release's namespace, the `appVersion` wins over image tags and
  labels on the same release line (major.minor): a pod a patch behind the
  release, or one without a version tag, is the release. An image or label
  version on another line is a second install in that namespace, judged
  at its own version: an istioctl canary revision running
  `istio/pilot:1.28.10` beside an `istiod` release at 1.31.1 in
  `istio-system` gets the 1.28 line's findings, and so does an image tag
  overridden in the release's values onto another line than the chart's
  `appVersion`.
- **Pod labels**, for images no matcher knows (a rebuilt or renamed
  controller image). A pod whose `app.kubernetes.io/name`, `helm.sh/chart`
  chart name or `app.kubernetes.io/part-of` is an entry's ID or chart
  matcher is that add-on; the version is `app.kubernetes.io/version` when
  the name label (or, without one, the chart label) named it. Pods running
  a provider build are never claimed through their labels.
- **IngressClass.** An `IngressClass` whose `spec.controller` is
  `k8s.io/ingress-nginx` is Ingress NGINX, without a version, when nothing
  else found Ingress NGINX, its RKE2 or AKS builds, or Traefik (whose
  Kubernetes Ingress NGINX provider serves that class). It is enough for a
  blocker, because Ingress NGINX is retired as a whole.
- **Node container runtimes.** `node.status.nodeInfo.containerRuntimeVersion`
  (`containerd://1.7.27`). Only containerd has an entry: other runtimes
  (`cri-o://`, `docker://`) are reported as an `addon-no-data/<runtime>`
  info finding naming their nodes, because their end of life and
  Kubernetes compatibility are not assessed.

Rendered manifests (`scan --files`, and `/gate`, which with `?cluster=`
merges the add-ons it finds into the cluster's) are matched the same way,
from the images and labels of Pod, Deployment, DaemonSet, StatefulSet,
ReplicaSet, Job and CronJob pod templates and from IngressClasses. Images
injected at admission time are not in them.

Image repositories that match no entry are listed as `unrecognizedImages`
(at most 200) in the inventory and the report, in JSON, the table, Markdown
and the dashboard's cluster view (not in SARIF), and never become
findings: an add-on the registry does not know is not judged. An add-on
running one of them may still have been found by its labels or Helm
release.

## How it is judged

At the installed version, independent of the target unless noted:

- the product is retired as a whole (`support.status: eol`, Ingress NGINX),
  or its installed release line has ended → `eol-addon` blocker;
- the installed version is older than the oldest release line the entry
  tracks, and that line has ended (cert-manager 1.5, whose oldest tracked
  line is 1.10; Istio 1.5, oldest 1.7) → `eol-addon` blocker keyed
  `eol-addon/<id>/below-<oldest line>`, citing that line and the product's
  lifecycle pages. endoflife.date stops at some old line, and anything
  older than an ended line has ended too;
- the end of life is within 90 days → `eol-approaching` warning;
- the release line's, or a compatibility row's, Kubernetes range excludes
  the **target** → `chart-incompat` blocker;
- a node runtime's ended release line, or a runtime older than the oldest
  tracked line → warning only, naming the nodes (the runtime comes with
  the node image, not with Kubernetes);
- no lifecycle data for the installed version (a version between two
  tracked lines, one newer than the newest tracked line, which means the
  registry is behind upstream, or a product without release lines), or no
  version readable → `addon-no-data` info, never a blocker.

Each install (one per namespace, two where a Helm release and images on
another release line share one, or one per node for a runtime) is judged
on its own, grouped by release line: with Istio 1.28 in one team's
namespace and 1.29 in another's, the ended 1.28 line is a finding keyed
`eol-addon/istio/1.28` that names only the namespaces and teams running
1.28, so a newer install neither hides an older one nor shares its blame. A product retired as a
whole is one finding naming every install. A node runtime's
`chart-incompat` blocker always lists the nodes that cannot run the target
(at most 10 named, the rest counted), since nodes have no namespace to
name them by.

## Evidence is tenant-controlled

Every source above except node runtimes and IngressClasses is written by
whoever can write to the namespace: Helm release Secrets and ConfigMaps,
pod images and pod labels. A tenant with `secrets: create` in its own
namespace can forge a Helm release, and one that creates pods chooses
their images and labels, so findings are only as trustworthy as namespace
write access. A forged release of chart `ingress-nginx` raises the
Ingress NGINX blocker for that namespace, and with it the cluster's
verdict. Hiding is narrower. A release's `appVersion` stands only for
pods on its own release line in its namespace (the **Helm releases**
bullet above), so a forged release that claims a newer version does not
hide an older image of the add-on on another line: that image is judged
at its own version. What it can still hide, in its own namespace, is a
patch-level difference on the same line, a pod whose image has no
version tag, and an add-on whose image no matcher recognizes.
The [security model](../operations/security-model-and-rbac.md#findings-are-only-as-trustworthy-as-namespace-write-access)
says what this means for a multi-tenant cluster.

Within one namespace, pods without a Helm release behind them (or off its
release line) are one install at their oldest version: a namespace running
Istio 1.28 and 1.29 images and no release is judged on the 1.28 line, and
the 1.29 line, being newer, is not judged separately there. Data-plane pods
that lag the control plane by a release line in the release's namespace
(an ingress gateway still on 1.28 beside `istiod` 1.31) are judged on
their own line too: that is the version running.

## What is in it

20 add-ons today:

| Add-on (`id`) | Release lines | Kubernetes ranges | Synced from endoflife.date |
|---|---|---|---|
| `argo-cd` | yes | — | yes |
| `calico` | yes | — | yes |
| `cert-manager` | yes | — | yes |
| `cilium` | yes | — | yes |
| `containerd` (node runtime) | yes | compat rows | yes |
| `etcd` | yes | — | yes |
| `flux` | yes | — | yes |
| `istio` | yes | per release line | yes |
| `keda` | yes | per release line | yes |
| `kyverno` | yes | per release line | yes |
| `traefik` | yes | — | yes |
| `ingress-nginx` | retired as a whole (March 2026) | — | — |
| `external-dns` | — | compat rows | — |
| `rke2-ingress-nginx` | — | compat rows | — |
| `aks-app-routing-nginx` | product end of life 2026-11-30 | — | — |
| `coredns` | — | — | — |
| `kube-state-metrics` | — | — | — |
| `metrics-server` | — | — | — |
| `prometheus-operator` | — | — | — |
| `velero` | — | — | — |

So: 11 entries carry release-line EOL data kept in sync with
[endoflife.date](https://endoflife.date), two carry an end-of-life date for
the product as a whole (Ingress NGINX, retired; the AKS application routing
NGINX build), 6 carry Kubernetes compatibility ranges, and for the 5 entries with
none of these, a detected install is reported as `addon-no-data` (info)
rather than judged. ExternalDNS and RKE2's ingress have compatibility
ranges but no end-of-life data, which is reported the same way. The
registry is small on purpose: every row needs a source.

## Citations are enforced

`registry.Validate` fails an entry with any status but `unknown`, any
release line and any compatibility row that has no citation URL, so an EOL
claim cannot land without a source. Entries with an `endoflife_product`
slug have their release lines generated by `tools/eol-sync` from the
endoflife.date API; CI runs `eol-sync -check` on pull requests that touch
`registry/`, and the weekly refresh opens a pull request when upstream data
moves. Like the rest of the knowledge base, a registry change reaches users
only in a release.

## Add an add-on

[`registry/CONTRIBUTING.md`](https://github.com/abd-ulbasit/upgradescope/blob/main/registry/CONTRIBUTING.md)
has the schema, how matchers and versions work, and the checks a pull
request must pass. The data's license is in
[`registry/DATA-LICENSE.md`](https://github.com/abd-ulbasit/upgradescope/blob/main/registry/DATA-LICENSE.md).
