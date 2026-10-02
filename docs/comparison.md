# Comparison

Several tools answer parts of "what breaks when this cluster moves to the
next Kubernetes minor?". This page says what each does that upgradescope
does not, and the other way round, with a source for every claim about
another tool (checked 2026-10-02; tools change, so check their current docs
before deciding). Corrections are welcome as issues.

## In one table

| | Deprecated / removed APIs | Add-on end of life | Version skew | Continuous | Fleet view |
|---|---|---|---|---|---|
| **pluto** | manifests, Helm releases, in-cluster resources | no | no | no (CLI) | no |
| **kubent** | manifests, last-applied annotations, Helm releases | no | no | no (CLI) | no |
| **kubepug** | live cluster or manifests, against a target's data | no | no | no (CLI) | no |
| **Nova** | no | outdated and deprecated Helm charts, outdated images | no | no (CLI) | no |
| **EKS cluster insights** | yes | EKS-managed add-on compatibility | kubelet, kube-proxy | yes, every 24 h | no (per cluster) |
| **GKE deprecation insights** | requests to deprecated APIs | no | no | yes | per project and location |
| **AKS** | blocks a minor upgrade on recent deprecated API use | no | no | at upgrade time | — |
| **Chkk** (commercial) | breaking changes and incompatibilities, curated (see below) | see below | see below | yes (SaaS) | yes |
| **upgradescope** | written objects, manifests, Helm releases, apiserver requests | 20 add-ons, cited | kubelet, kube-proxy, control plane | yes (agent) | yes (server) |

Every cell is expanded below. "Continuous" means it re-evaluates without
someone re-running it.

## pluto (Fairwinds)

[pluto](https://github.com/FairwindsOps/pluto) finds deprecated and removed
apiVersions in manifests and Helm charts (`detect-files`), in Helm releases
in a cluster (`detect-helm`), in resources running in a cluster
(`detect-api-resources`, which reads the `last-applied-configuration`
annotation), and both of the latter at once (`detect-all-in-cluster`)
([quickstart](https://pluto.docs.fairwinds.com/quickstart/)). It has CI exit
codes and a GitHub Action.

- **pluto, not upgradescope:** a lighter single-purpose binary; a long
  record in CI pipelines.
- **upgradescope, not pluto:** judges live objects by their
  `managedFields` writers, not only by the last-applied annotation (which
  server-side apply and many GitOps tools do not write); reads the
  apiserver's record of deprecated requests; add-on EOL, version skew and
  chart `kubeVersion`; a verdict that says when it could not see enough; a
  continuous agent and a fleet server.

## kubent (kube-no-trouble)

[kubent](https://github.com/doitintl/kube-no-trouble) checks a live cluster
once, through three collectors: local manifest files, the
`kubectl.kubernetes.io/last-applied-configuration` annotation of live
objects, and Helm v3 releases stored as Secrets or ConfigMaps. It prints
text, JSON or CSV (`--output`), and `--exit-error` fails a pipeline on
findings ([arguments](https://github.com/doitintl/kube-no-trouble#arguments)).

- **kubent, not upgradescope:** a small single-purpose binary; CSV
  output.
- **upgradescope, not kubent:** detection from `managedFields` (objects
  never applied with client-side `kubectl apply` have no last-applied
  annotation); apiserver request data; add-on EOL, skew, chart
  compatibility; SARIF; continuous and fleet-wide.

## kubepug

[kubepug](https://github.com/kubepug/kubepug) (also `kubectl deprecations`
through krew) downloads deprecation data for a chosen Kubernetes release and
checks a running cluster, or manifests passed to it, against it, with
`--error-on-deprecated` and `--error-on-deleted` for CI. The data is a
`data.json` it fetches from `https://kubepug.xyz/data/data.json` by default,
regenerated hourly; `--database` points it at another copy, for air-gapped
use ([README](https://github.com/kubepug/kubepug#readme)).

- **kubepug, not upgradescope:** a kubectl plugin in krew today (upgradescope's
  krew listing is pending); its data is fetched per run, so it is not
  tied to a binary release.
- **upgradescope, not kubepug:** the same differences as above:
  authorship-based detection, request data, add-ons, skew, a verdict, the
  agent and the server.

## Nova (Fairwinds)

[Nova](https://github.com/FairwindsOps/nova) scans the Helm releases in a
cluster against Helm repositories and reports charts that are outdated or
deprecated, and can report outdated container images.

- **Nova, not upgradescope:** "a newer version exists", for any chart in a
  repository you can reach; upgradescope's registry knows end-of-life
  dates for 20 add-ons, not the newest version of every chart.
- **upgradescope, not Nova:** end of life rather than "outdated" (a
  supported old line is fine, an unsupported one blocks), API removals,
  skew and a verdict per target.

## The cloud providers' own checks

- **EKS** runs [upgrade insights](https://docs.aws.amazon.com/eks/latest/userguide/cluster-insights.html)
  on every cluster, refreshed every 24 hours (viewed, and listed through
  the `ListInsights` API, one cluster at a time), against an Amazon-curated list
  of upgrade-impacting issues; rollback-readiness insights cover API usage,
  kubelet and kube-proxy skew and EKS-managed add-on compatibility.
- **GKE** shows deprecation insights based on observed calls to deprecated
  APIs, and pauses automatic minor upgrades while it sees them
  ([Feature and API deprecations](https://docs.cloud.google.com/kubernetes-engine/docs/deprecations));
  the insights are listed per project and cluster location
  ([viewing them](https://docs.cloud.google.com/kubernetes-engine/docs/deprecations/viewing-deprecation-insights-and-recommendations)).
- **AKS** can block a minor control-plane upgrade when it has seen
  deprecated API use within 12 hours, with an override window
  ([docs](https://learn.microsoft.com/en-us/azure/aks/stop-cluster-upgrade-api-breaking-changes)).

What they have that upgradescope does not: the provider's own request logs
(GKE and AKS see who calls a deprecated API across the whole control plane),
knowledge of their managed add-ons, and no installation. What upgradescope
adds: the same answer on every provider and on self-managed clusters, before
a pull request merges (manifests in CI), third-party add-on end of life, an
exportable report per target, and one fleet view across providers. On a
managed cluster, use both.

## Chkk

[Chkk](https://www.chkk.io/) is a commercial platform (SaaS, with a free
tier and a CLI) for cloud-native lifecycle management: an Upgrade Copilot
that researches and plans upgrades, a risk ledger of breaking changes and
incompatibilities, agents that open infrastructure-as-code pull requests,
and a catalog of the clusters, add-ons and dependencies it finds, covering
hundreds of open-source projects.

- **Chkk, not upgradescope:** far broader curated knowledge of add-ons and
  their breaking changes, upgrade plans and generated pull requests, and a
  hosted service.
- **upgradescope, not Chkk:** Apache-2.0 source, self-hosted, with nothing
  leaving your network; every knowledge-base claim is public and cited.

Disclosure: upgradescope's author interned at chkk.io. upgradescope is
clean-room: no proprietary code, data, schemas or documents were used or
consulted, and the knowledge base comes only from upstream source and
public, cited pages ([background research](research.md)).

## When to use something else

- Only deprecated and removed APIs, in one CI step: pluto, kubent or
  kubepug are smaller, single-purpose binaries.
- "Is there a newer chart?": Nova.
- On a single managed cluster, the provider's insights are already there;
  upgradescope adds pre-merge CI gating, third-party add-ons and a
  cross-provider fleet view.
