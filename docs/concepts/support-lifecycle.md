# Managed-provider support and what waiting costs

On EKS, GKE and AKS a cluster that stays on a Kubernetes minor after the
provider's standard support for it ends moves into extended support (paid on
EKS and GKE) or, on AKS, into Long Term Support if it is enabled and
otherwise, for one minor only, into platform support. Nothing in the
cluster changes when that happens, so teams find out from the bill. Extended
support is automatic only on EKS; on GKE and AKS it is opt-in, and the
scanner cannot see a cluster's release channel or AKS tier, so it states
the provider's window and the condition it applies under, never that
your cluster is enrolled (see below). upgradescope
dates it: **the day your cluster's minor enters extended support, and, where
the provider publishes a price, what that adds per cluster per year.**

## What it reports

For a cluster on EKS, GKE or AKS whose Kubernetes minor the
[knowledge base](knowledge-base.md) has dates for, the `support-lifecycle`
finding is:

| Phase | When | Finding |
|---|---|---|
| `standard` | standard support does not end within 90 days | none |
| `ending` | standard support ends within 90 days | **warning** |
| `extended` | from the day standard support ends, until the provider's extended window ends | **blocker** |
| `ended` | extended support has ended, or the provider offered none for the minor | **blocker** |

For EKS 1.34 on 2026-10-15, `scan` prints (the detail is one line in the
terminal and is wrapped here):

```text
[support-lifecycle] Kubernetes 1.34 leaves Amazon EKS standard support on 2026-12-02
    Amazon EKS ends standard support for Kubernetes 1.34 on 2026-12-02; extended
    support then runs until 2027-12-02, when Amazon EKS stops supporting it.
    Extended support costs $4,380 more per cluster per year at list price ($0.60
    against $0.10 per cluster-hour, over 8,760 hours; list price as of
    2026-10-03, not your bill). Extended support is on by default and billed per
    cluster-hour; a cluster whose upgrade policy is STANDARD is upgraded
    automatically at the end of standard support instead.
    fix: Upgrade the control plane, one minor at a time. The nearest minor in standard support is 1.35; the newest known is 1.36.
```

On GKE and AKS the same finding is worded conditionally, because the
dataset knows the provider's window and not whether this cluster is in it.
For AKS 1.33 on 2026-10-04:

```text
[support-lifecycle] Kubernetes 1.33 is past Azure Kubernetes Service standard support (ended 2026-07-31)
    Azure Kubernetes Service ended standard support for Kubernetes 1.33 on
    2026-07-31; extended support, which applies only if Long Term Support is
    enabled, runs until 2027-07-31; upgradescope cannot see whether this
    cluster is enrolled. AKS has no automatic extended support. Without Long
    Term Support (which needs the Premium tier), a cluster past community
    support gets only platform support (Azure and AKS platform issues, no
    Kubernetes fixes) while its minor is N-3, one behind the oldest minor in
    community support. Older minors are out of support.
    fix: Upgrade the control plane, one minor at a time. The nearest minor in standard support is 1.34; the newest known is 1.36.
```

and on a GKE cluster, "extended support, which applies only if the cluster
is on the Extended release channel", with the price followed by "It is
charged only for clusters on the Extended release channel", since other
channels are upgraded automatically. The `scan` line and the
`ClusterReadiness` status carry the same caveat
(`extendedSupportCondition`, `annualCostNote`), so a `supportPhase` of
`extended` on GKE or AKS means the provider's window, not a confirmed
enrolment. Treat it as a prompt to check the cluster's channel or tier.

The 90 days are the same window as the add-on
[end-of-life warning](addon-registry.md): long enough to schedule an upgrade
(two or three minors a year) and to put a dated cost in front of whoever
approves the spend. The finding is judged against the scan's clock, so the
same inventory gives the same report on the same date. Its title carries the
date, never "in N days", so a stored report does not change every day.

Extended support begins **on the date shown, at the start of that day
(UTC)**. EKS documents billing as starting then; GKE and AKS state the date
standard support ends and do not state the hour, so for them the finding can
be off by a day.

The status is also reported when there is no finding: `scan` prints a
`Support:` line under `Server:` in every phase, `--output json` carries it as
`support` (see the [JSON report](../reference/json-report.md)), and the
agent writes it to the `ClusterReadiness` status as `supportPhase`,
`extendedSupportFrom`, `extendedSupportEnds` where the provider offers
extended support, and, with a price, `annualCostDelta`, `currency`,
`priceAsOf` and `annualCostNote`, plus `extendedSupportCondition` where
extended support is opt-in (see the [CRD reference](../reference/crd.md)). The server's
report endpoint carries `support` and the finding too. Those fields are the
cluster's, the same for every target. The dashboard lists the finding like
any other; it has no support or fleet-cost view yet.

It is a blocker because a cluster past standard support is not on a version
the provider fully supports. It is judged whatever the target, and it blocks
`--fail-on blocker` gates until the cluster is upgraded; accept it with an
[ignore rule](../guides/suppressions-and-baselines.md) (`key:
support-lifecycle/eks/1.34`, or `category: support-lifecycle`) when
you have decided to stay and pay.

## The cost line

The annual figure is `(extended − standard) × 8760` at the provider's
**published list price**, labelled with the day the price was read. It is
never your bill: prices change, and contracts, committed-use discounts and
regions differ.

| Provider | Price in the knowledge base | Figure |
|---|---|---|
| EKS | $0.10 standard, $0.60 extended per cluster-hour ([AWS pricing](https://aws.amazon.com/eks/pricing/)) | $4,380 per cluster per year |
| GKE | $0.10 standard, $0.60 extended per cluster-hour; the extra $0.50 is charged only to clusters on the **Extended release channel** ([GKE pricing](https://cloud.google.com/kubernetes-engine/pricing)). The finding, the `scan` line and the status (`annualCostNote`) say so. | $4,380 per cluster per year, for those clusters |
| AKS | none. Long Term Support needs the Premium tier and Microsoft's pages defer to a pricing page that shows no number a citation can point at; the difference would also depend on the tier the cluster is on. | no cost line, dates only |

A provider whose price is not cited, a cluster whose provider is not known,
and a minor the dataset has no dates for get no cost line; a cluster with no
provider gets no finding at all. A number is never inferred. After extended
support ends nothing more can be billed for the minor, so the figure is not
shown in phase `ended`.

What "extended support" means differs, and the finding states it:

- **EKS**: on by default, billed per cluster-hour from the start of the day
  standard support ends. A cluster whose upgrade policy is `STANDARD` is
  upgraded automatically at the end of standard support instead.
- **GKE**: opt-in, for clusters on the Extended release channel; clusters on
  other channels are upgraded automatically at the end of standard support.
  The dataset's `extended_support_condition` carries this.
- **AKS**: opt-in. There is no automatic extended support. Without Long
  Term Support (which needs the Premium tier), a cluster past community
  support gets only platform support (Azure and AKS platform issues, no
  Kubernetes fixes), and only while its minor is N-3, one behind the oldest
  minor in community support; Microsoft's platform-support policy ends
  when the cluster drops to N-4, and older minors are out of support. The
  dates in the dataset are the community-support end and the LTS end, not
  the end of platform support, so a cluster two or more minors past
  community support with no LTS is out of support whatever phase the
  dataset shows.

## How the provider is known

The collector sets `Inventory.Provider` (`eks`, `gke`, `aks` or `other`)
from signals only the provider produces, and never guesses:

| Provider | Claimed by |
|---|---|
| `eks` | a server version like `v1.34.2-eks-3abc123` (a commit hash; EKS Distro's and EKS Anywhere's `-eks-1-29-12` is not the managed service), or a node label `eks.amazonaws.com/nodegroup` or `eks.amazonaws.com/compute-type` |
| `gke` | a server version like `v1.34.2-gke.1234000`, or a node label `cloud.google.com/gke-nodepool` |
| `aks` | a node label `kubernetes.azure.com/cluster` (AKS adds nothing to the version) |

A node's `providerID` scheme (`aws://`, `gce://`, `azure://`) is never a
claim, because kubeadm on EC2 or on Azure VMs has the same: it can only
contradict one (a `-gke.` version on vSphere nodes is GKE on-prem, not GKE).
A node with no `providerID` at all contradicts nothing, so a `-gke.` version
on nodes that carry none (some Google Distributed Cloud bare-metal clusters)
is claimed as GKE. Two providers' claims, a contradiction, or no claim at
all give `other`. When the nodes could not be listed, or the list failed
partway (the pages read are not evidence), an EKS or GKE version suffix
still names the provider, and a cluster the version does not name is left
undetermined rather than called `other`. Manifests (`--files`) have no provider.

Only an agent that sends `provider` gets a support status from the server:
agents from v0.1.x and v0.2.0's release candidates push none, so for their
clusters the server reports no support line or cost until the agent is
upgraded to v0.2.0 or later. A server that does not know a provider name
an agent sends (a newer agent that learned another service) accepts the
snapshot and reports no support for it.

## Where the dates come from

`registry/data/providers/{eks,gke,aks}.yaml`: for each Kubernetes minor,
the end of standard support and the end of extended support, with citations,
and the hand-entered price with its as-of date and its source.

- EKS and AKS versions are generated from
  [endoflife.date](https://endoflife.date/amazon-eks) by `tools/eol-sync`,
  and CI fails a pull request that touches `registry/` when they drift, as
  for the add-ons.
- GKE is hand-curated from [Google's release schedule](https://docs.cloud.google.com/kubernetes-engine/docs/release-schedule):
  endoflife.date has no extended-support end for it and its standard-support
  dates differ from Google's. Minors whose dates are still a month or quarter
  estimate are left out.
- A minor the file does not list gets no finding.

See [CONTRIBUTING](https://github.com/abd-ulbasit/upgradescope/blob/main/registry/CONTRIBUTING.md#managed-provider-support-calendars)
to correct a date or add a cited price.
