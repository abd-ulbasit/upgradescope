# Background research (June 2026, corrected October 2026)

Compiled 2026-06-10 from public sources, as the record of the problem
upgradescope set out to solve and of the adjacent tools checked first so as
not to rebuild them. Corrected 2026-10-02 after an audit ([#23](https://github.com/abd-ulbasit/upgradescope/issues/23)) found that
the first version understated pluto and kubent and made claims without
sources. Every factual claim below links its source; the lines marked
*opinion* are the author's reading, not facts. For the current, sourced
tool-by-tool comparison, see [Comparison](comparison.md).

## The gap, as it looked

Open-source deprecation scanners are **point-in-time CLIs**. pluto checks
manifests and Helm charts in repositories, Helm releases in a cluster, and
in-cluster resources through their last-applied annotations
([pluto quickstart](https://pluto.docs.fairwinds.com/quickstart/), and the
[FAQ](https://pluto.docs.fairwinds.com/faq/) on why that annotation is
unreliable); kubent
checks a live cluster once through file, last-applied-annotation and Helm v3
collectors ([kube-no-trouble](https://github.com/doitintl/kube-no-trouble));
kubepug checks a live cluster or manifests against a target release's data
([kubepug](https://github.com/kubepug/kubepug)). They cover deprecated and
removed APIs, not add-on end of life or version skew. Continuous checks
exist as commercial products ([Chkk](https://www.chkk.io/)) and as the cloud
providers' own upgrade insights for their managed clusters
([EKS](https://docs.aws.amazon.com/eks/latest/userguide/cluster-insights.html),
[GKE](https://docs.cloud.google.com/kubernetes-engine/docs/deprecations),
[AKS](https://learn.microsoft.com/en-us/azure/aks/stop-cluster-upgrade-api-breaking-changes)).
*Opinion:* the open-source gap is a self-hosted tool that runs continuously
on any cluster, covers add-on end of life with cited data, and rolls a fleet
up for an auditor. Radar's upgrade-impact view (an interactive UI, with a
hosted multi-cluster product) covers part of the API and skew ground; see
[Comparison](comparison.md).

## Evidence

### 1. Add-on end of life is a real category

- Ingress NGINX, the most widely deployed ingress controller, was retired
  in March 2026: no further releases, bug fixes or security fixes
  ([kubernetes.io, November 2025](https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/)).
- Compliance frameworks treat end-of-life software in the data path as a
  finding ([Chkk's write-up of the retirement](https://www.chkk.io/blog/ingress-nginx-deprecation)).
- Migration tooling exists ([ingress2gateway](https://github.com/kubernetes-sigs/ingress2gateway));
  *opinion:* detecting what still runs EOL software, continuously, is the
  part left to each team.

### 2. Deprecated-API detection: what the open-source scanners read

- pluto's `detect-files` and `detect-helm` read manifests and Helm releases;
  `detect-api-resources` reads in-cluster resources
  ([quickstart](https://pluto.docs.fairwinds.com/quickstart/)). Its FAQ says
  the last-applied annotation it relies on there is not a reliable way to
  detect deprecated APIs on a live cluster: a `kubectl patch` removes it
  ([pluto FAQ](https://pluto.docs.fairwinds.com/faq/)).
- Radar (Skyhook, Apache-2.0) has an *Upgrade impact* check that reads the
  apiserver's `apiserver_requested_deprecated_apis` series, among other
  evidence
  ([Radar README](https://github.com/skyhook-io/radar#kubernetes-upgrade-impact),
  [guide](https://radarhq.io/docs/features/upgrade-impact)). Its code's first commit is dated 2026-08-03
  ([history](https://github.com/skyhook-io/radar/commits/main/pkg/upgradereadiness)),
  after this note's June snapshot, so the gap opinion above is narrower than
  it first read.
- kubent's live-cluster collectors read the
  `kubectl.kubernetes.io/last-applied-configuration` annotation and Helm v3
  release objects ([kube-no-trouble](https://github.com/doitintl/kube-no-trouble)).
- The last-applied annotation is written only by client-side `kubectl
  apply`, so objects created by server-side apply or most controllers do
  not carry it; and the apiserver serves every stored object at every
  served version, so a list at a deprecated version does not show who uses
  it ([Kubernetes: server-side apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/),
  [API versioning](https://kubernetes.io/docs/reference/using-api/#api-versioning)).
  upgradescope reads `managedFields` instead
  ([Deprecated-API detection](concepts/api-usage-detection.md)).
- Practitioner guides recommend running several of these tools around
  each upgrade ([Plural](https://www.plural.sh/blog/kubernetes-api-deprecation-tool-guide/),
  [OneUptime](https://oneuptime.com/blog/post/2026-02-09-identify-deprecated-apis-upgrades/view)).

### 3. The commercial benchmark, and the disclosure that goes with it

- Chkk sells upgrade planning (Upgrade Copilot), a risk ledger of breaking
  changes and incompatibilities, and a catalog of clusters and add-ons
  ([chkk.io](https://www.chkk.io/)).
- **Disclosure:** the author interned at chkk.io. upgradescope is
  clean-room: no proprietary code, data, schemas or internal documents were
  used or consulted. Every knowledge-base entry is generated from upstream
  `k8s.io/api` source or carries a public citation URL, and CI checks both.
- *Opinion:* most API lifecycle data is machine-derivable from upstream
  Kubernetes, and end-of-life data for the most common add-ons is a
  maintainable curated registry; that is the part an open-source tool can
  do well.

## Adjacent tools (not rebuilt)

- **ingress2gateway**: migration; upgradescope detects and recommends.
- **Goldilocks, KRR**: rightsizing, a different category.
- **Trivy, kube-bench**: vulnerability and CIS scanning; the same shape
  (scan, findings), a different knowledge domain.
- **pluto, kubent, kubepug, Nova**: see [Comparison](comparison.md).
