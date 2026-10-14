# Roadmap

Where upgradescope is going from October 2026 into 2027, as four
milestones. Dates are intentions, not promises; the issues in each
[milestone](https://github.com/abd-ulbasit/upgradescope/milestones) are the
current plan, the pinned
[tracking issue #426](https://github.com/abd-ulbasit/upgradescope/issues/426)
has the assessment behind it, and a change to the plan is made there, in the
open, as [GOVERNANCE.md](GOVERNANCE.md#how-decisions-are-made) describes.

## Status: maintenance mode (since 15 October 2026)

upgradescope is maintained, not expanded. The evidence since October 2026
points one way:

- No external users, and no issue from outside the project, in the weeks
  after v0.2.0.
- krew-index declined the plugin on 2026-10-13 as overlapping with an
  existing one ([kubernetes-sigs/krew-index#6411](https://github.com/kubernetes-sigs/krew-index/pull/6411)), and other
  tools now cover most of a live-cluster upgrade check
  ([comparison](docs/comparison.md)).
- The core problem is shrinking: in the
  [Helm chart survey](docs/research/helm-chart-survey-2026-10/README.md),
  none of the 42 most-starred charts that rendered uses a removed or
  deprecated API at Kubernetes 1.37.

What continues: the weekly knowledge-base and registry refresh, Dependabot
updates and CI; Kubernetes 1.38 data on its release day ([#407](https://github.com/abd-ulbasit/upgradescope/issues/407)) and a
release when the data changes; security fixes, bug fixes and answers to
issues.

What is on hold, labelled
[`on-hold`](https://github.com/abd-ulbasit/upgradescope/issues?q=is%3Aopen+label%3Aon-hold):
the v0.4.0 features and most of the CNCF-readiness work. They come back if
users ask for them: open an issue, or add a 👍 to an on-hold one.

What is still active: the survey write-up ([#413](https://github.com/abd-ulbasit/upgradescope/issues/413), [#415](https://github.com/abd-ulbasit/upgradescope/issues/415)), the
contributions to the projects upgradescope overlaps with ([#419](https://github.com/abd-ulbasit/upgradescope/issues/419)), and
data corrections for endoflife.date ([#408](https://github.com/abd-ulbasit/upgradescope/issues/408)).

## Where upgradescope stands

As of 2026-10-15: v0.3.0 is published (12 October 2026). It is distributed
through the Homebrew tap, the signed image and OCI Helm chart (Artifact Hub,
as a Verified Publisher), the GitHub Action on GitHub Marketplace, and the
release archives and packages. krew-index declined the plugin on
2026-10-13 ([#6411](https://github.com/kubernetes-sigs/krew-index/pull/6411)).
There are no external users yet: [ADOPTERS.md](ADOPTERS.md) is empty, and no
issue has come from outside the project.

The direction follows from an assessment of the field in October 2026.
Removed-API detection is a commodity: pluto, kubent and kubepug do it, the
managed providers report live callers for free, and since Kubernetes 1.32
upstream has removed only alpha and beta APIs. What is harder, and what
users ask, is about add-ons: is this add-on end of life, will its version
run on my next Kubernetes, and are its CRD versions still served? And the
cheapest way to try upgradescope is the Action, which needs no install.
So the next releases put add-on depth first, then go looking for users;
the fleet server, dashboard, team rollups, auditor exports, MCP and the
score stay, but are not where new effort goes until users ask for them.
That plan is paused: see the status above.

## 1. v0.3.0: add-on data, README and governance

Where it stands: 34 add-ons in the registry, each claim cited; 15 synced
from endoflife.date, 15 with Kubernetes ranges, and 4 with no lifecycle
data. Due 16 October 2026 ([milestone](https://github.com/abd-ulbasit/upgradescope/milestone/4)).

- **Cited Kubernetes ranges for the add-ons people run most**, not only an
  end-of-life date ([#394](https://github.com/abd-ulbasit/upgradescope/issues/394)).
- **A README that leads with the add-on wedge**: end of life, unserved CRD
  versions, stored Helm releases and pull-request gating, with the audit
  detail on the docs site ([#404](https://github.com/abd-ulbasit/upgradescope/issues/404)).
- **Governance files**: GOVERNANCE, MAINTAINERS, ADOPTERS and this
  roadmap ([#409](https://github.com/abd-ulbasit/upgradescope/issues/409)).

## 2. v0.4.0: upgrade preflight and add-on depth (on hold)

On hold since 15 October 2026, with no due date ([milestone](https://github.com/abd-ulbasit/upgradescope/milestone/5)).

- **Upgrade preflight**: drain-blocking PodDisruptionBudgets, webhooks and
  APIServices with no ready backend, and nodes not Ready, which
  upgradescope does not check today ([#397](https://github.com/abd-ulbasit/upgradescope/issues/397)).
- **More add-ons, and more of each**: toward the 50 most-deployed, with a
  good-first-issue path for adding one ([#395](https://github.com/abd-ulbasit/upgradescope/issues/395)), and an add-on's
  tested-with table as a non-blocking "untested on the target" finding
  ([#396](https://github.com/abd-ulbasit/upgradescope/issues/396)).
- **Fewer false blockers, more of the upgrade covered**: a custom resource
  tied to an unserved CRD version only by an old `managedFields` entry is a
  warning ([#398](https://github.com/abd-ulbasit/upgradescope/issues/398)); upstream support dates for kind, kubeadm and RKE2
  clusters ([#399](https://github.com/abd-ulbasit/upgradescope/issues/399)); field-level removals at the target, with config
  breakers a stated non-goal ([#401](https://github.com/abd-ulbasit/upgradescope/issues/401)).
- **Files mode and Helm**: `--files` lists every add-on it matched, passing
  ones included ([#400](https://github.com/abd-ulbasit/upgradescope/issues/400)), and a decision on Helm findings without a
  cluster-wide Secrets read ([#403](https://github.com/abd-ulbasit/upgradescope/issues/403)).
- **Docs for people switching**: migrating from kube-no-trouble, pluto and
  kubepug, a comparison that includes kube-upgrade-check ([#405](https://github.com/abd-ulbasit/upgradescope/issues/405)), and
  one page per upgrade problem ([#406](https://github.com/abd-ulbasit/upgradescope/issues/406)).
- **Add-on upgrades themselves**: which CRD versions each add-on release
  serves, so an add-on upgrade cannot strand custom resources
  ([#427](https://github.com/abd-ulbasit/upgradescope/issues/427)), and, for an end-of-life or incompatible add-on, the
  supported line or upstream successor to move to ([#428](https://github.com/abd-ulbasit/upgradescope/issues/428)).

## 3. Launch: first users

Where it stands: published and installable, with no users yet. In
maintenance mode only the write-up, its sharing, 1.38 readiness and the
review stay active; the rest is on hold. From December 2026 to February 2027; the adoption review is on 7 December 2026
([#417](https://github.com/abd-ulbasit/upgradescope/issues/417), [milestone](https://github.com/abd-ulbasit/upgradescope/milestone/6)).

- **A first article from real data**: the most-starred Artifact Hub charts
  scanned against Kubernetes 1.37 ([#412](https://github.com/abd-ulbasit/upgradescope/issues/412)), the write-up ([#413](https://github.com/abd-ulbasit/upgradescope/issues/413)), and
  sharing it with practitioner communities ([#415](https://github.com/abd-ulbasit/upgradescope/issues/415)).
- **Kubernetes 1.38 readiness on its release day** ([#407](https://github.com/abd-ulbasit/upgradescope/issues/407)).
- (on hold) **Places people can talk to the project**: Discussions, a "Who is using
  upgradescope?" thread and an add-on request template ([#410](https://github.com/abd-ulbasit/upgradescope/issues/410)), and
  listings where people look for tools ([#414](https://github.com/abd-ulbasit/upgradescope/issues/414)).
- (on hold) **Talks** at local CNCF community groups and KCDs, and the KubeCon NA
  2027 CFP ([#416](https://github.com/abd-ulbasit/upgradescope/issues/416)).
- (on hold) **Three teams outside the project** running it on real clusters
  ([#411](https://github.com/abd-ulbasit/upgradescope/issues/411)); what they ask for decides what comes after this roadmap.
- **The krew-index submission**: declined on 2026-10-13 as overlapping
  with an existing plugin
  ([kubernetes-sigs/krew-index#6411](https://github.com/kubernetes-sigs/krew-index/pull/6411)); recorded in [#75](https://github.com/abd-ulbasit/upgradescope/issues/75).

## 4. CNCF readiness

Where it stands: Apache-2.0, an OpenSSF Scorecard, signed releases, a
security policy, a code of conduct, and, with this roadmap, governance,
maintainer and adopter files. One maintainer, independent. Through 2027,
due 30 September 2027 ([milestone](https://github.com/abd-ulbasit/upgradescope/milestone/7)).
On hold, except the contributions to overlapping projects and to
endoflife.date ([#419](https://github.com/abd-ulbasit/upgradescope/issues/419), [#408](https://github.com/abd-ulbasit/upgradescope/issues/408)).

- **Contributor sign-off** (DCO) on every commit and the **OpenSSF Best
  Practices badge** ([#422](https://github.com/abd-ulbasit/upgradescope/issues/422)).
- **A contributor ladder and two maintainers from other employers**
  ([#421](https://github.com/abd-ulbasit/upgradescope/issues/421)), and a decision on moving to a neutral GitHub organization
  ([#423](https://github.com/abd-ulbasit/upgradescope/issues/423)).
- **Working with the field**: contributions to the overlapping CNCF
  projects ([#419](https://github.com/abd-ulbasit/upgradescope/issues/419)) and Kubernetes-compatibility data for endoflife.date
  ([#408](https://github.com/abd-ulbasit/upgradescope/issues/408)); removed and locked feature gates read from control-plane
  static pods ([#402](https://github.com/abd-ulbasit/upgradescope/issues/402)); how upgradescope is built and verified
  ([#425](https://github.com/abd-ulbasit/upgradescope/issues/425)).
- **TAG Operational Resilience and the Day-0 General Technical Review**
  ([#420](https://github.com/abd-ulbasit/upgradescope/issues/420)), the CNCF Landscape once the repository has 300 stars
  ([#418](https://github.com/abd-ulbasit/upgradescope/issues/418)), and **adopters** willing to be named in
  [ADOPTERS.md](ADOPTERS.md).
- **An application to the CNCF Sandbox**, once the bar is met ([#424](https://github.com/abd-ulbasit/upgradescope/issues/424)).

## Issues by milestone

As of 2026-10-15: the v0.3.0 issues are closed, and items marked
(on hold) carry the `on-hold` label. The pinned
[tracking issue #426](https://github.com/abd-ulbasit/upgradescope/issues/426)
links them all.

### [v0.3.0: add-on data, README and governance](https://github.com/abd-ulbasit/upgradescope/milestone/4)

- [#394](https://github.com/abd-ulbasit/upgradescope/issues/394) feat(registry): cited Kubernetes compatibility ranges for the add-ons people run most (done)
- [#404](https://github.com/abd-ulbasit/upgradescope/issues/404) docs: README first screen around the add-on wedge; move audit detail to the docs site (done)
- [#409](https://github.com/abd-ulbasit/upgradescope/issues/409) governance: MAINTAINERS, GOVERNANCE, ADOPTERS and ROADMAP files (done)

### [v0.4.0: upgrade preflight and add-on depth](https://github.com/abd-ulbasit/upgradescope/milestone/5)

- [#395](https://github.com/abd-ulbasit/upgradescope/issues/395) feat(registry): grow the registry toward the 50 most-deployed add-ons, with a good-first-issue path (on hold)
- [#396](https://github.com/abd-ulbasit/upgradescope/issues/396) feat(registry): show an add-on's tested-with table as a non-blocking "untested on the target" finding (on hold)
- [#397](https://github.com/abd-ulbasit/upgradescope/issues/397) feat(engine): upgrade preflight: drain-blocking PDBs, webhooks and APIServices with no ready backend, nodes not Ready (on hold)
- [#398](https://github.com/abd-ulbasit/upgradescope/issues/398) fix(engine): a custom resource linked to an unserved CRD version only by an old managedFields entry is a warning, not a blocker (on hold)
- [#399](https://github.com/abd-ulbasit/upgradescope/issues/399) feat(engine): upstream Kubernetes support dates for clusters whose provider is other (kind, kubeadm, RKE2) (on hold)
- [#400](https://github.com/abd-ulbasit/upgradescope/issues/400) feat(cli): files mode lists every add-on it matched, with the rendered file, including the ones that pass (on hold)
- [#401](https://github.com/abd-ulbasit/upgradescope/issues/401) feat(kb): field-level removals at the target version, and config breakers as a stated non-goal (on hold)
- [#403](https://github.com/abd-ulbasit/upgradescope/issues/403) needs-decision: Helm findings without cluster-wide Secrets read (on hold)
- [#405](https://github.com/abd-ulbasit/upgradescope/issues/405) docs: migrating from kube-no-trouble, pluto and kubepug, and a refreshed comparison that includes kube-upgrade-check (on hold)
- [#406](https://github.com/abd-ulbasit/upgradescope/issues/406) docs: one page per upgrade problem, and the docs site's search and share metadata (on hold)

### [Launch: first users](https://github.com/abd-ulbasit/upgradescope/milestone/6)

- [#407](https://github.com/abd-ulbasit/upgradescope/issues/407) kb: Kubernetes 1.38 readiness on its release day
- [#410](https://github.com/abd-ulbasit/upgradescope/issues/410) community: GitHub Discussions, a "Who is using upgradescope?" thread, and an add-on request template (on hold)
- [#411](https://github.com/abd-ulbasit/upgradescope/issues/411) adoption: three teams outside the project run upgradescope on real clusters (on hold)
- [#412](https://github.com/abd-ulbasit/upgradescope/issues/412) research: the Helm chart survey, the most-starred Artifact Hub charts against Kubernetes 1.37 (done)
- [#413](https://github.com/abd-ulbasit/upgradescope/issues/413) launch: publish the survey write-up
- [#414](https://github.com/abd-ulbasit/upgradescope/issues/414) launch: list upgradescope where people look for tools (on hold)
- [#415](https://github.com/abd-ulbasit/upgradescope/issues/415) launch: share the survey with practitioner communities
- [#416](https://github.com/abd-ulbasit/upgradescope/issues/416) launch: talks at local CNCF community groups and KCDs, and the KubeCon NA 2027 CFP (on hold)
- [#417](https://github.com/abd-ulbasit/upgradescope/issues/417) adoption review on 2026-12-07

### [CNCF readiness](https://github.com/abd-ulbasit/upgradescope/milestone/7)

- [#402](https://github.com/abd-ulbasit/upgradescope/issues/402) feat(kb): removed and locked feature gates, read from control-plane static pods (on hold)
- [#408](https://github.com/abd-ulbasit/upgradescope/issues/408) investigate: contribute Kubernetes-compatibility data and date corrections to endoflife.date
- [#418](https://github.com/abd-ulbasit/upgradescope/issues/418) cncf: CNCF Landscape listing once the repository has 300 stars (on hold)
- [#419](https://github.com/abd-ulbasit/upgradescope/issues/419) cncf: contribute to the overlapping CNCF projects
- [#420](https://github.com/abd-ulbasit/upgradescope/issues/420) cncf: present to TAG Operational Resilience and complete the Day-0 General Technical Review (on hold)
- [#421](https://github.com/abd-ulbasit/upgradescope/issues/421) governance: a contributor ladder, and two maintainers from other employers (on hold)
- [#422](https://github.com/abd-ulbasit/upgradescope/issues/422) governance: DCO sign-off and the OpenSSF Best Practices badge (on hold)
- [#423](https://github.com/abd-ulbasit/upgradescope/issues/423) needs-decision: move the repository to a neutral GitHub organization (on hold)
- [#424](https://github.com/abd-ulbasit/upgradescope/issues/424) cncf: apply to the CNCF Sandbox when the bar is met (on hold)
- [#425](https://github.com/abd-ulbasit/upgradescope/issues/425) docs: how upgradescope is built and verified (on hold)

All open milestones and their issues:
https://github.com/abd-ulbasit/upgradescope/milestones
