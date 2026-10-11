# Verdict and score

Every report, from `scan`, the agent or the server, carries two answers:
a **verdict** for gating and a **score** for trends. Both come from the same
pure function, `engine.Evaluate(inventory, knowledge base, target, date)`.

## The verdict

| Verdict | When |
|---|---|
| `blocked` | At least one finding is a blocker. |
| `unknown` | No blocker, but a **required** check was not assessed, so a blocker may have been missed. |
| `ready` | No blocker, and every required check was assessed. |

`ready` in every output is `verdict == "ready"`; it is kept for clients of
v0.1, when it meant "no blockers". A failed required check never leaves a
report reading ready.

A check the inventory does not report at all (its `capabilities` map has
no entry for it, as in a hand-built, third-party or empty inventory) was
not assessed, and for a required check below that is a required gap too:
`api-usage` always, `versions` and `addons` in a cluster inventory. The
server judges such a push `unknown` at best; every collector since v0.1.0
reports them.

Required checks:

- **`api-usage`**, always: which removed or deprecated APIs objects use.
  A *partial* api-usage (some resources could not be listed) is required
  only when what it skipped includes an API removed at or before the target.
- **`kb-coverage`**, always: the target must be at or below the knowledge
  base's horizon. A newer target cannot be judged: the knowledge base does
  not know what that release removes.
- **`target`**, for live clusters: the target must be an upgrade. A
  target at or below the minor the oldest kube-apiserver already runs (a
  downgrade or the same minor) is reported as a required `target` gap, so
  the verdict is `unknown`, never `ready`: every check judges a newer minor.
  A target below 1.16, the oldest minor the knowledge base covers (a typo
  such as `1.4` for `1.40`, which is what YAML makes of an unquoted number),
  is not a verdict: it is refused as an input.
- **`versions`**, for live clusters: the server version (skew needs it).
  A *partial* `versions` is required too when it names a component whose
  version upstream would have told and was not read (a component image
  with a digest or `latest` for a tag, or any unread kube-apiserver,
  kube-controller-manager or kube-scheduler pod): that component may be
  the one past the skew policy. A kube-proxy pod on a vendor image of
  another name (Oracle OKE's) is an optional gap. A *partial* `versions`
  that skipped `nodes` (no Node was listed, so kubelet skew and node
  runtimes were not assessed) is required too. See
  [Version skew](version-skew.md).
- **`addons`**, for live clusters: add-on detection from images, labels,
  IngressClasses and charts. Without the cluster-wide pod list the
  capability is *partial* and still required: the Helm releases and
  IngressClasses that were read are matched (an EOL chart-installed
  ingress-nginx still blocks), but an add-on installed any other way goes
  undetected, so a clean result is `unknown`. A role that cannot list
  IngressClasses alone leaves an optional partial gap. The agent lists
  the pods outside `kube-system` only every few ticks and detects add-ons
  from the last full pass in between (`--pod-pass-every`,
  `--pod-pass-max-age`); that is no gap, since the pass was complete, and
  the report's `addOnEvidenceAgeSeconds` says how old it was. See
  [Scale and cost](../operations/scale.md).

Not required, because a blocker cannot hide behind them, or because managed
platforms routinely deny them: `deprecated-calls` (the apiserver's
`/metrics`, whether it is denied or does not answer in time), `helm`
(Secrets are often forbidden; the objects themselves are still checked by
api-usage, and add-ons by their images and labels) and `crds` (CRD
versions are the add-ons', not the Kubernetes target's; see
[CRD versions](api-usage-detection.md#crd-versions)). In files mode
`api-usage` and `kb-coverage` are required and add-ons are assessed from
the manifests without being required (an image injected at admission is
not in them); `versions`, `deprecated-calls` and `helm` are reported with
the reason `files mode`.

## What a verdict does not cover

`ready` means that no blocker was found among what the engine judges, and
that every required check ran. The engine judges the `apiVersion` and `kind`
of each object against the API lifecycle data, and the in-tree volume
plugins that pods, PersistentVolumes and StorageClasses name ([Volume plugins](volume-plugins.md)).
It does **not** assess other field-level removals inside an API that is
still served: an object that uses the seccomp alpha annotations,
`Service.spec.externalIPs` or `beta.kubernetes.io/os` scans as `ready` with a
score of 100. The table and Markdown reports end with a line saying so
("Field-level removals other than in-tree volume plugins are not checked by
this version."). This is a disclosure, not a
check: it changes no score, verdict or exit code, and the JSON report does
not carry it.

## Severity, by category

Severity depends on the category. Only some categories depend on the
target.

| Category | Blocker | Warning | Info |
|---|---|---|---|
| `removed-api` | An object (or a Helm release's stored manifest) uses an API removed at or before the target; or, in `--files` mode and the gate (the manifests are proposed state), a manifest uses an API version the target does not serve yet, titled "not served until X" (applying it fails the same way). | The API is removed in the minor after the target. A removal after the knowledge base's horizon is titled "(projected)" in either severity. | — |
| `deprecated-api` | — | A Helm release's stored manifest uses a deprecated API that is not removed by the target, even when the live scan also flags the object (as info). | An object uses an API that is deprecated (or will be, after the target) and not removed by the minor after the target. |
| `deprecated-api-in-use` | The apiserver saw requests to an API removed at or before the target, and no object finding covers it (or every object of the one that did was suppressed by annotation or an object-scoped rule). The removal release is the knowledge base's when it knows the API, else the metric's `removed_release` label. | Removed in the minor after the target. | No removal release known to the knowledge base or reported. |
| `eol-addon` | The add-on, or its installed release line, is past end of life. **Whatever the target.** | A node container runtime past end of life (it ships with the node image, not with Kubernetes). | — |
| `eol-approaching` | — | End of life within 90 days. | — |
| `chart-incompat` | The installed release line's (or a registry compat range's) Kubernetes range excludes the target; a Helm chart's `kubeVersion` excludes the target. | — | A chart `kubeVersion` that does not parse. |
| `version-skew` | Kubelets or kube-proxy that would fall outside the policy once the control plane is at the target; a controller-manager or scheduler newer than the apiserver (**whatever the target**). | Violations today: kubelets or kube-proxy too far behind, kubelets or kube-proxy newer than the apiserver, HA apiserver spread, controller-manager or scheduler too far behind. | Unparseable kubelet versions; a target more than one minor ahead, which takes several upgrades (`version-skew/upgrade-path` names each step). |
| `kb-stale` | — | The cluster or the target is newer than the knowledge base's horizon. | — |
| `addon-no-data` | — | — | A detected add-on whose version has no lifecycle data. |
| `unknown-api` | — | — | An object of a built-in API group (core, or any group the generator's scheme registers (`k8s.io/api` plus the apiextensions and apiregistration schemes), such as `imagepolicy.k8s.io`) at a version or kind the knowledge base does not know, such as the typo `apps/v1beta9`: whether the target serves it was not assessed. API groups of CRDs produce nothing. |
| `support-lifecycle` | A cluster on EKS, GKE or AKS whose Kubernetes minor is past the provider's standard support (in extended support, or out of support). **Whatever the target.** | Standard support for the minor ends within 90 days. | — |
| `crd-version` | Custom resources at a version their CRD does not serve (`served: false`, or no longer listed): the apiserver rejects them. **Whatever the target.** | Custom resources written through a version the CRD marks `deprecated: true`; a `status.storedVersions` entry the CRD no longer serves. | A deprecated CRD version nothing was found using. |
| `volume-plugin` | A pod, pod template or PersistentVolume names an in-tree volume plugin removed with no migration path at or before the target (`glusterfs`, `cephfs`, `rbd`; `gitRepo`, disabled from 1.33), or a StorageClass names its in-tree provisioner (`kubernetes.io/rbd`): pods naming it, or mounting a claim bound to such a PersistentVolume, do not start, and the class's new claims are not provisioned. | Such a plugin removed in the minor after the target; a plugin served only through CSI migration at the target (`awsElasticBlockStore`, `gcePersistentDisk`, ...), naming the CSI driver that must be installed. Never a blocker, since a cluster with the driver is fine. | A plugin deprecated only (`flexVolume`), or whose removal or CSI-only minor is later. See [in-tree volume plugins](volume-plugins.md). |

[Version skew](version-skew.md) has the skew rules; the
[add-on registry](addon-registry.md) the EOL data, and
[managed-provider support](support-lifecycle.md) the `support-lifecycle` finding
and its cost line.

## The score

```text
score = max(0, 100 − min(75, 25 × blockers) − min(20, 5 × warnings))
```

Info findings are never scored, and suppressed findings are excluded. The
caps keep one noisy category from zeroing the score, and keep a cluster with
many warnings distinguishable from one with a blocker. The score is for
trends and comparison: **gate on the verdict**, not on a score threshold.
Per-team scores apply the same formula to each team's findings.

Each team also has a verdict, on the report's rules: `blocked` by a
blocker of its own or one no team is attributed (it cannot be ruled out as
the team's, and it blocks the cluster's upgrade either way), otherwise
`unknown` when a required check was not assessed (it may have hidden a
blocker of any team), otherwise `ready`. A team's `ready` is true only when
its verdict is `ready`, so no team reads ready on an `unknown` cluster.
Another team's blocker leaves it `ready`; gate on the cluster's verdict,
not on one team's. A team with no findings is not listed, and there is no
per-namespace score.

The formula is part of the public contract
([compatibility policy](../compatibility-policy.md)): changing it is a
breaking change.

## Deterministic, for a given date

The same inventory, knowledge base, target **and date** give the same
report, byte for byte (the golden tests in `internal/engine` pin it). The
date matters because end-of-life is calendar-based: an add-on whose end of
life is 2026-11-30 turns from a warning (`eol-approaching`) into a blocker
(`eol-addon`) on that day, with nothing in the cluster changing. End-of-life
dates are compared at 00:00 UTC on the date. Two other inputs move without
any change in the cluster: the knowledge base (with each release) and the
default target (when the cluster is upgraded).

Consequences:

- The agent re-evaluates every tick, so a passing EOL date reaches the
  `ClusterReadiness` within one interval.
- The server re-evaluates stored snapshots hourly and just after each UTC
  midnight; until it has, reads mark a stored verdict `outdated`.
- A CI gate against a server (`POST /api/v1/gate?cluster=`) or with a
  floating action version can change its answer on an unchanged pull
  request. Pin the version for a reproducible gate.

## The gate

`scan --fail-on blocker|warning|never` (default `blocker`) exits 2 when a
finding at or above the threshold remains after suppression and is not
`unchanged` against a `--baseline`, **or** when the verdict is `unknown`,
unless `--allow-incomplete`. A target that is not an upgrade of the cluster
exits 2 even with `--allow-incomplete`; only `--fail-on never` always exits 0,
that case included. Operational errors, and a `--target` below the knowledge
base, exit 1. The server's gate endpoint and the GitHub Action use the same rule.
Details: [Suppressions and baselines](../guides/suppressions-and-baselines.md#how-the-gate-decides).
