# upgradescope

upgradescope answers one question about a Kubernetes cluster, or a
directory of rendered manifests: **what blocks the move to the next
Kubernetes minor?** It checks removed and deprecated APIs (in stored objects,
in manifests, and in requests the apiserver still receives), add-ons past
end of life, version skew outside the upstream policy, and Helm charts that
declare they do not support the target. The answer is a verdict (`ready`,
`blocked` or `unknown`), a 0–100 score, and the findings with their fixes,
as a table, JSON, SARIF or Markdown, an exit code for CI, and a
`ClusterReadiness` object in the cluster.

!!! note "These docs describe v0.2.0"
    The site follows `main`, which ships as v0.2.0, the first release CI
    builds, signs and publishes. Until v0.2.0 is tagged, what pins it (the
    GitHub Action `@v0.2.0`, release downloads, the container image and the
    OCI chart) does not resolve yet. To try what these pages describe,
    build `main`
    (`go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@main`);
    the chart in a clone defaults to the image of the release it was cut
    for, so to run `main` set an image you build from it.
    [Install](operations/install.md) says what v0.1.x lacks.

It is one Apache-2.0 binary with three modes:

| Mode | Runs | You get |
|---|---|---|
| `upgradescope scan` | once, on a laptop or in CI | a report and an exit code |
| `upgradescope agent` | in the cluster, every 10 minutes | a `ClusterReadiness` object kept current, metrics, optional pushes to a server |
| `upgradescope serve` | anywhere | history, a fleet matrix, team rollups, a CI gate endpoint, auditor exports, notifications and a dashboard |

## Who it is for

- **Platform teams planning an upgrade**: one scan says what to fix first,
  and the agent says when it is done.
- **Application teams in CI**: a pull request that adds a removed API fails
  before it merges, with the finding on the line that caused it.
- **Anyone answering an auditor**: an exported, cited report per cluster and
  target, with the score history behind it.

## 30-second tour

```console
$ upgradescope scan --files rendered/ --target 1.37
upgradescope upgrade readiness report

Cluster:  files
Target:   1.37
KB:       k8s.io/api v0.37.1; lifecycle 696a4b81; registry de96a5da

SCORE  50/100
READY  no

BLOCKER (2)
  [removed-api] batch/v1beta1 CronJob removed in 1.25 (1 object)
      1 manifest object(s) use this API: namespace unset (1).
      - nightly  rendered/all.yaml:12
      fix: migrate to batch/v1 CronJob
      see: https://kubernetes.io/docs/reference/using-api/deprecation-guide/
  ...
readiness gate failed: findings at or above --fail-on threshold
$ echo $?
2
```

(The `KB:` digests are illustrative; your build prints its own.)

Against a live cluster, drop `--files`: the same scan then also reads which
objects were *written* through a deprecated API version, the add-ons
actually running (from pod images and labels, Helm releases and
IngressClasses), the kubelet and control-plane versions, and the apiserver's own record of deprecated
requests. [CLI in two minutes](getting-started/cli.md) walks through it.

## What it does not do

- It changes nothing in your cluster. The agent's only writes are its own
  `ClusterReadiness` object (and, by default, that object's CRD); see
  [Security model and RBAC](operations/security-model-and-rbac.md).
- It does not upgrade anything or open pull requests.
- It does not read apiserver audit logs, so it cannot say *which client*
  still calls a deprecated API, nor check `kubectl` client skew.
- Its knowledge base is compiled into the binary and changes only with a
  release; see [Knowledge base](concepts/knowledge-base.md).

Every public claim the project makes is listed, with the tests that prove
it, in the [claims ledger](claims.md). How it compares with pluto, kubent,
kubepug, Nova and the cloud providers' own checks is in
[Comparison](comparison.md).
