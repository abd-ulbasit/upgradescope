# Upgrade plans for multi-minor upgrades

The control plane is upgraded one minor version at a time, and managed
services (EKS, GKE, AKS) enforce that. A cluster on 1.31 going to 1.36 is
therefore five upgrades: 1.32, 1.33, 1.34, 1.35, then 1.36. A plain
`scan --target 1.36` judges the cluster once, against 1.36. It tells you
everything that blocks 1.36, but not which of those blockers you have to
fix before the 1.32 upgrade and which can wait until 1.35.

`scan --plan` answers that. It judges the cluster at every minor on the way
and lists each finding at the first upgrade it affects.

## Running it

On a live cluster, the plan starts at the minor the cluster runs (its
oldest kube-apiserver, the same version `--target` is judged against):

```sh
upgradescope scan --target 1.36 --plan
```

Rendered manifests carry no cluster version, so `--files` needs `--from`,
the minor the cluster runs now:

```sh
upgradescope scan --files rendered/ --from 1.31 --target 1.36 --plan
```

`--plan` works with `--output table` (the default), `markdown` and `json`.
SARIF has no place for upgrade steps, so `--plan --output sarif` is an
error; run the SARIF scan without `--plan`.

## Reading the plan

A 1.31 cluster with a `resource.k8s.io/v1alpha3` DeviceClass (removed in
1.34) and a node whose kubelet runs 1.31. The table shows the plan after
the score and verdict, before the findings:

```text
UPGRADE PLAN  1.31 → 1.36 in 5 upgrades; each finding is listed at the first upgrade it affects
  1.31 → 1.32  ready    0 blockers, 0 warnings, 1 info
      + info     [deprecated-api] resource.k8s.io/v1alpha3 DeviceClass deprecated in 1.34, after target 1.32 (1 object)
  1.32 → 1.33  ready    0 blockers, 1 warning, 0 info
      + warning  [removed-api] resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object)
  1.33 → 1.34  blocked  1 blocker, 0 warnings, 0 info
      ~ warning → blocker  resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object) (listed at 1.33)
  1.34 → 1.35  blocked  2 blockers, 0 warnings, 0 info
      + blocker  [version-skew] 1 node(s) would exceed kubelet version skew after upgrading to 1.35
      1 carried from earlier upgrades
  1.35 → 1.36  blocked  2 blockers, 0 warnings, 0 info
      nothing new; 2 carried from earlier upgrades
```

Read it as follows:

- The first two upgrades are clear. The DeviceClass has to be migrated
  before the control plane reaches 1.34.
- The node has to be upgraded before 1.35. Each upgrade judges the cluster
  as it is now: kubelet and kube-proxy skew is checked against that
  upgrade's control-plane version, so you can see which upgrade the nodes
  have to catch up before.
- Each line has the upgrade's verdict and counts. The counts include
  findings carried from earlier upgrades.
- `+` is a finding first seen at this upgrade.
- `~` is a finding listed at an earlier upgrade (named in parentheses)
  whose severity changed. A finding can be a warning at one upgrade and a
  blocker at a later one; the plan shows both.
- Findings that are already listed and keep their severity are carried,
  and only counted.

The findings after the plan are the report at the final target, with full
detail: objects, teams, remediation and citations. Every blocker in the
plan's last upgrade is among them.

A finding is identified by its [key](suppressions-and-baselines.md#finding-keys).
An API on its way out changes key once: it is a `deprecated-api` info
until the release before its removal, then a `removed-api` warning, and a
`removed-api` blocker from its removal on. That is why the DeviceClass
above is a new finding at 1.33 rather than a change, and why the info stops
being carried.

`--output markdown` renders the plan as one table row per upgrade, with the
findings it adds and the severity changes, above the findings table.

## JSON

`--output json` adds `hops`, one object per upgrade
([field reference](../reference/json-report.md#fields)):

```json
{
  "from": "1.33",
  "to": "1.34",
  "score": 75,
  "verdict": "blocked",
  "findings": [],
  "changed": [
    {
      "key": "removed-api/resource.k8s.io/v1alpha3/DeviceClass",
      "severity": "blocker",
      "was": "warning",
      "title": "resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object)",
      "since": "1.33"
    }
  ]
}
```

`findings` holds the findings first seen at that upgrade, in full.
`changed` and `carried` refer to findings listed at an earlier upgrade by
`key`, with their severity and title at this one and `since`, the upgrade
that lists them in full. `notAssessed` lists the upgrade's gaps. Everything
outside `hops` is the report at `--target`, exactly as without `--plan`.
`hops` is an addition within `schemaVersion` 1.

## Ignore rules, baselines and the gate

- Ignore rules and object annotations apply to every upgrade, so a
  suppressed finding is in no upgrade of the plan, just as it is not in
  the report.
- `--plan` never changes the exit code. The verdict, the score and the
  `--fail-on` gate are those of `--target`, and the plan's last upgrade
  always has the same blockers as the report. A property test checks this
  over hundreds of random inventories (`TestPlanFinalHopMatchesEvaluate`).
- `--baseline` marks the report's findings, not the plan's.

## What the plan assumes

- **The cluster stays as it is.** Each upgrade judges today's objects,
  add-on versions and node versions against that upgrade's control-plane
  version. The plan shows what you must change before each upgrade; it
  does not assume you changed anything.
- **One minor at a time.** The knowledge base can hold upgrade paths that
  skip minors, each with a citation for the provider documentation that
  allows it, and a plan takes such a path where one applies. It ships none.
  Whether AKS long-term-support versions may skip minors has not been
  verified, so no provider-specific path is included. To add one, open a
  pull request with the citation ([contributing](../contributing.md)).
- **A live cluster's version comes from the cluster.** When the scan
  cannot read a kube-apiserver version, it prints a warning and reports
  the target alone, without a plan.

The dashboard, the agent and the `ClusterReadiness` status do not show
plans yet; `scan --plan` is the only way to get one.
