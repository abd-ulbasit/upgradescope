# JSON report

`upgradescope scan --output json` writes the report as one JSON object;
`--write-baseline` writes the same document to a file, and the GitHub
Action's `report-json` output points to it. It is the contract for CI
pipelines and tools built on the scan.

The schema is
[`api/report.schema.json`](https://github.com/abd-ulbasit/upgradescope/blob/main/api/report.schema.json)
(JSON Schema draft 2020-12). `TestJSONReportMatchesSchema` validates real
scan output against it, with findings, suppressed findings, a baseline,
not-assessed gaps and unrecognized images, and fails on any field the
schema does not list.

## Versioning

`schemaVersion` is `1`. Within a schema version, fields are only added:
never renamed, removed, retyped or given a new meaning. Ignore fields you do
not know; the schema allows them for that reason. Finding categories may be
added too (`unknown-api` was), so the schema gives `category` as a string
and lists the known ones in its description: handle a category you do not
know by its `severity`. The `severity`, `verdict` and `baselineState`
values are fixed. A breaking change bumps
`schemaVersion` and is listed in the changelog
([compatibility policy](../compatibility-policy.md)). `toolVersion` names
the binary that wrote the report and carries no compatibility meaning.

The server's report-shaped responses lead with the same two fields: a
cluster's report (`GET /api/v1/clusters/{id}/report`, stored or what-if)
and the gate's JSON answer (`POST /api/v1/gate`). There `toolVersion` is
the serving server's build, also for an evaluation an older server stored
([REST API](api.md)).

## Example

`scan --files rendered --target 1.37 --output json` on one
`networking.k8s.io/v1beta1` Ingress (the test validates this example too):

```json
{
  "schemaVersion": 1,
  "toolVersion": "0.2.0",
  "filesBase": "rendered",
  "clusterId": "files",
  "target": "1.37",
  "kbVersion": "k8s.io/api v0.37.1; lifecycle 696a4b81; registry de96a5da",
  "score": 75,
  "ready": false,
  "verdict": "blocked",
  "findings": [
    {
      "category": "removed-api",
      "severity": "blocker",
      "key": "removed-api/networking.k8s.io/v1beta1/Ingress",
      "title": "networking.k8s.io/v1beta1 Ingress removed in 1.22 (1 object)",
      "detail": "1 manifest object(s) use this API: shop (1).",
      "namespaces": ["shop"],
      "remediation": "migrate to networking.k8s.io/v1 Ingress",
      "citations": ["https://kubernetes.io/docs/reference/using-api/deprecation-guide/"],
      "objects": [{"namespace": "shop", "name": "web", "file": "all.yaml", "line": 1}]
    }
  ],
  "notAssessed": [
    {"capability": "deprecated-calls", "reason": "files mode"},
    {"capability": "helm", "reason": "files mode"},
    {"capability": "versions", "reason": "files mode"}
  ],
  "teams": {"unattributed": {"score": 75, "ready": false, "verdict": "blocked", "blockers": 1, "warnings": 0}}
}
```

## Fields

| Field | Meaning |
|---|---|
| `schemaVersion` | `1`. |
| `toolVersion` | The `upgradescope` version that wrote it. |
| `filesBase` | `--files` only: the scanned directory that object `file` paths are relative to; relative to the working directory when inside it, absolute otherwise. |
| `clusterId` | The cluster's `kube-system` namespace UID, or `files`. |
| `target` | The target minor. |
| `serverVersion` | Live scans only: the kube-apiserver's `gitVersion` (`v1.34.2-gke.100`), the version the target was judged against. |
| `kubeContext` | Live scans only: the kubeconfig context the scan read. |
| `apiServer` | Live scans only: the API server the scan read, as scheme, host and port (`https://10.0.0.1:6443`); never the kubeconfig's credentials, path or query. |
| `kbVersion` | The knowledge base the report was judged with. |
| `score` | 0–100 ([formula](../concepts/verdict-and-score.md#the-score)). |
| `verdict` | `ready`, `blocked` or `unknown` ([rules](../concepts/verdict-and-score.md#the-verdict)). |
| `ready` | `verdict == "ready"`, kept for v0.1 readers. |
| `findings[]` | Sorted by severity, then category, then title. Each has `category` ([list](../concepts/verdict-and-score.md#severity-by-category)), `severity`, `key` (stable across runs: baselines and notifications match on it), `title`, `detail`, and where they apply `teams` (of every affected namespace), `namespaces` (at most 100, with `namespacesOmitted`), `remediation`, `citations`, `objects` (at most 100, with `objectsOmitted`) and `baselineState` (`new` or `unchanged`, with `--baseline`). |
| `notAssessed[]` | What the scan could not see: `capability`, `reason`, and `required` when the gap makes the verdict unknown, `partial` and `skipped` when a capability read only part of what it covers. |
| `suppressed[]` | Findings an ignore rule or annotation accepted, with `reason`, `source` and `expires`. Not in the score or verdict. |
| `unrecognizedImages[]` | Image repositories (`host/path`, no tag) that no add-on image matcher recognised, sorted, at most 200. A gap in add-on detection, not a finding: not in the score or verdict, and an add-on running one may still have been found by its labels or Helm release. |
| `unrecognizedImagesOmitted` | How many more unrecognized repositories there were beyond the 200 listed. |
| `support` | A cluster on EKS, GKE or AKS whose Kubernetes minor the knowledge base dates: `provider`, `minor`, `phase` (`standard`, `ending`, `extended`, `ended`), `extendedSupportFrom` (the day extended support begins) and `extendedSupportEnds`, and, only where the provider's price is cited, `annualCostDelta` (what extended support adds per cluster per year, a decimal string), `currency` and `priceAsOf`: a list price as of that day, not a bill, with `annualCostNote` when the provider charges it only to some clusters. Where the provider's extended support is opt-in (GKE, AKS) `extendedSupportCondition` names the configuration it applies under, and phases `ending` and `extended` then describe the provider's window, not a confirmed enrolment. Present whether or not it is a finding; absent for other clusters and manifests. The same for every target. See [managed-provider support](../concepts/support-lifecycle.md). |
| `hops[]` | `scan --plan` only: the [upgrade plan](../guides/upgrade-plan.md), one entry per control-plane upgrade from the cluster's minor (or `--from`) to `target`, in order. Each has `from`, `to`, the `score` and `verdict` at `to`, `findings` (the ones first seen at this upgrade, in full), `changed` and `carried` (findings listed at an earlier upgrade, by `key`, with their `severity` and `title` here, `since`, the upgrade that lists them, and in `changed`, `was`, the earlier severity) and `notAssessed`. Every other field is the report at `target`, the verdict included. |
| `teams` | Per-team scores; findings without a team are under `unattributed`. A team's `verdict` is `blocked` by a blocker of its own or by an unattributed one (it cannot be ruled out as the team's), otherwise `unknown` when the report has a required not-assessed gap, otherwise `ready`; another team's blockers do not lower it, and `ready` is `verdict == "ready"`. `score`, `blockers` and `warnings` count only the team's own findings. |

The server's report endpoint serves the same report fields (except
`kubeContext`, `apiServer` and `hops`, which only `scan` sets), plus where the
report came from ([`GET /api/v1/clusters/{id}/report`](api.md#clusters));
the SARIF output carries the same findings as results, keyed by `key`.
