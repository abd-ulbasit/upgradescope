# upgradescope

[![CI](https://github.com/abd-ulbasit/upgradescope/actions/workflows/ci.yml/badge.svg)](https://github.com/abd-ulbasit/upgradescope/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/abd-ulbasit/upgradescope)](https://github.com/abd-ulbasit/upgradescope/releases)
[![Docs](https://img.shields.io/badge/docs-site-blue)](https://abd-ulbasit.github.io/upgradescope/)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/abd-ulbasit/upgradescope/badge)](https://scorecard.dev/viewer/?uri=github.com/abd-ulbasit/upgradescope)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

upgradescope tells you what blocks a Kubernetes cluster, or a directory of
rendered manifests, from moving to the next minor: APIs that are removed or
deprecated at the target (found by who still writes them, and by what the
apiserver is still asked for), add-ons past end of life, version skew
outside the upstream policy, Helm charts that exclude the target, and
[in-tree volume plugins](https://abd-ulbasit.github.io/upgradescope/concepts/volume-plugins/)
(`glusterfs`, `gitRepo`, `awsElasticBlockStore`) that the target removed or
serves only through a CSI driver. The
answer is a verdict (`ready`, `blocked` or `unknown` when a required check
could not run), a 0–100 score and cited findings, as a table, JSON, SARIF,
Markdown, JUnit or GitLab Code Quality, an exit code for CI, and a
`ClusterReadiness` object an in-cluster agent keeps current. A self-hosted server adds history, a fleet view, team
rollups, auditor exports and [Slack or optionally signed webhook notifications](https://abd-ulbasit.github.io/upgradescope/operations/#notifications)
when readiness changes. One Apache-2.0 binary. `scan` only reads; the
agent writes nothing but its own `ClusterReadiness` object and that CRD's
schema.

**Documentation: https://abd-ulbasit.github.io/upgradescope/** ·
every public claim, with the test that proves it: [claims ledger](docs/claims.md)

![upgradescope scanning rendered manifests](docs/img/demo.gif)

## Install

| | |
|---|---|
| Homebrew (from v0.2.0) | `brew install abd-ulbasit/tap/upgradescope` |
| Go | `go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@latest` |
| Release archives | [Releases](https://github.com/abd-ulbasit/upgradescope/releases): linux, darwin (and windows from v0.2.0) on amd64/arm64, with `checksums.txt`, signed with cosign from v0.2.0 |
| Container image (from v0.2.0) | `ghcr.io/abd-ulbasit/upgradescope:<version>`, signed |
| Helm chart (from v0.2.0) | `oci://ghcr.io/abd-ulbasit/charts/upgradescope`, signed; before that, `deploy/chart` from a clone |

Verifying a download:

```sh
sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 -c --ignore-missing checksums.txt
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/abd-ulbasit/upgradescope/.github/workflows/release.yml@refs/tags/$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

[Install](https://abd-ulbasit.github.io/upgradescope/operations/install/)
has every channel, image and chart verification, and what v0.1.x lacks.

## Quickstart

**Scan** rendered manifests, or the cluster your kubeconfig points at:

```sh
upgradescope scan --files rendered/ --target 1.37
upgradescope scan --target 1.37
```

Exit code 0 means the gate passed, 2 that it failed (a blocker, or a verdict
of `unknown`), 1 an error. `--fail-on warning|never` moves the threshold;
`--allow-incomplete` (the Action's `allow-incomplete: true`) gates on
findings alone when a required check could not run, for example a target
newer than the knowledge base knows.
[CLI in two minutes](https://abd-ulbasit.github.io/upgradescope/getting-started/cli/).

**Gate pull requests** with the GitHub Action (pin it to a release tag or
its SHA; the floating `v0` tag moves to each newer stable release). For a
release from v0.2.0-rc.2 on, it installs the binary only once its build
provenance verifies: the release workflow built it at that tag
([what is checked](action/README.md#install-and-integrity)).

```yaml
      - run: helm template my-release ./chart --output-dir rendered
      - uses: abd-ulbasit/upgradescope@v0.2.0
        id: gate
        with: {path: rendered, target: "1.37", version: v0.2.0}
      - uses: github/codeql-action/upload-sarif@v4
        # also when the gate failed; not on a fork PR, whose token is read-only
        if: ${{ !cancelled() && steps.gate.outputs.sarif-file != '' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}
        with: {sarif_file: "${{ steps.gate.outputs.sarif-file }}"}
```

On a pull request from a fork the `if:` skips the upload, since a fork's
token cannot write code-scanning alerts: the gate still runs and fails the
job only when it fails, and the job summary and annotations are the report.
Never switch the trigger to `pull_request_target` to get the upload back: it
runs with a write token on files the pull request controls. If the gate runs
on Dependabot's pull requests, whose token is read-only too, add
`&& github.actor != 'dependabot[bot]'` to the condition.

Code scanning places an alert on a pull request's diff only when its file is
committed. Alerts for a render like `rendered/` above appear in the Security
tab, not on the diff.

[CI gate](https://abd-ulbasit.github.io/upgradescope/getting-started/ci-gate/)
also covers the server's gate endpoint. GitLab CI, Jenkins and Azure
Pipelines have templates in [`ci/`](ci/) that publish JUnit and GitLab Code
Quality reports (`--output junit|gitlab-codequality`): see
[Other CI systems](https://abd-ulbasit.github.io/upgradescope/guides/other-ci/).

**Run it in the cluster**:

```sh
helm install upgradescope deploy/chart -n upgradescope --create-namespace
kubectl get ucr        # NAME  TARGET  SCORE  READY  LASTEVALUATED  AGE
```

The agent re-evaluates every 10 minutes and writes a `ClusterReadiness`
object with a standard `Ready` condition, for `kubectl wait`, alerts,
GitOps health checks and policy engines: [examples](examples/) for Kyverno,
Gatekeeper and Renovate, in [Acting on readiness](https://abd-ulbasit.github.io/upgradescope/guides/acting-on-readiness/). A chart from a clone defaults to the image of the
release it was cut for; to run unreleased changes, build an image from the
clone and set `image.repository` and `image.tag`.
[In-cluster agent](https://abd-ulbasit.github.io/upgradescope/getting-started/in-cluster/) ·
[Fleet server](https://abd-ulbasit.github.io/upgradescope/getting-started/fleet/).

**Fleet**: agents in many clusters push to one `upgradescope serve` for the
fleet view, team rollups and the server-side CI gate. Serve it over TLS:
terminate at an ingress (`server.ingress`), or let `serve` terminate it
(`server.tls.secretName`; `--tls-cert-file`/`--tls-key-file`, re-read on
renewal), and point agents at a private CA with `agent.serverCA`. Over plain
http an agent's bearer token and inventories cross the network in cleartext.
[Exposing the server to remote agents](https://abd-ulbasit.github.io/upgradescope/getting-started/fleet/#exposing-the-server-to-remote-agents).

**Ask an AI assistant**: `upgradescope mcp` gives Claude Code (`claude mcp add upgradescope -- upgradescope mcp`), or any MCP client, read-only tools that return these reports; setup for Claude Desktop is in [AI assistants (MCP)](https://abd-ulbasit.github.io/upgradescope/getting-started/mcp/).

## What it checks, and where it stops

- **Removed and deprecated APIs**, in manifests and in live objects. Live
  objects count when someone still *writes* them through the deprecated
  version (from `managedFields`, then the last-applied annotation), not
  merely because the apiserver still serves that version, which would flag
  every cluster. An object with neither record cannot be attributed: it is
  an info finding, "authorship unknown", which never changes the verdict or
  score. [How](https://abd-ulbasit.github.io/upgradescope/concepts/api-usage-detection/).
- **Deprecated API requests**, from the apiserver's
  `apiserver_requested_deprecated_apis` metric: that a client asked, not
  which client, and only for the replica that answered. Managed control
  planes often forbid it; the report then says so.
- **Add-ons past end of life** and their Kubernetes compatibility, from a
  registry of 27 add-ons in which every claim carries a citation (Ingress
  NGINX, retired in March 2026, Kubernetes Dashboard, Promtail, Grafana Agent
  and Weave Net are blockers). An add-on is found by its
  container images, its Helm release, its `helm.sh/chart` or
  `app.kubernetes.io/*` pod labels, or an Ingress NGINX `IngressClass`; in
  rendered manifests, by the images and labels of workload pod templates.
  Add-ons outside the registry are not judged; the report lists the images
  no matcher recognised.
  [Registry](https://abd-ulbasit.github.io/upgradescope/concepts/addon-registry/).
- **Version skew** of kubelets, kube-proxy, controller-manager, scheduler
  and HA apiservers; not `kubectl` clients, which only audit logs reveal.
- **Helm charts** whose `kubeVersion` excludes the target, and stored
  release manifests that use removed APIs, from releases in Helm's secrets
  and configmaps drivers (not the sql driver). Charts deployed by Argo CD or
  Flux are found through the chart their Application or HelmRelease names
  (opt-in RBAC); Argo CD's `helm template` leaves no release, so for them
  those two checks are reported as not assessed.
  [GitOps](https://abd-ulbasit.github.io/upgradescope/guides/gitops-argo-flux/#charts-your-gitops-tool-deploys).

The verdict is `blocked` on any blocker, `unknown` when a required check
(API usage, the knowledge base covering the target, and for a live cluster
its versions and add-ons) was not assessed, and `ready` otherwise. For a
given inventory, knowledge base, target and **date** the result is the same;
end-of-life dates move verdicts on their own.
[Verdict and score](https://abd-ulbasit.github.io/upgradescope/concepts/verdict-and-score/).

The knowledge base is compiled into the binary and changes only with a
release; `upgradescope version` prints the newest Kubernetes minor it
covers. A target past that is `unknown`, by design.
[Knowledge base](https://abd-ulbasit.github.io/upgradescope/concepts/knowledge-base/).

**Not checked by this version**: field-level removals and in-tree
volume-plugin removals inside served APIs. The engine judges the
`apiVersion` and `kind` of an object, so a pod spec that uses a removed
volume plugin (`glusterfs`, `awsElasticBlockStore`, `cephfs`, `rbd`, among
others) or the deprecated `gitRepo` volume is not flagged, and a `READY` with
a score of 100 does not cover it. The table and Markdown reports say so in a
closing line; the score, verdict, exit code and JSON are not affected.

Tested against Kubernetes 1.24 to the newest minor: 1.29 and up as whole
kind clusters, 1.24 to 1.28 as real kube-apiservers through envtest (the
collector and engine only; 1.24 and 1.28 on every pull request, every
minor weekly).
[Tested Kubernetes range](https://abd-ulbasit.github.io/upgradescope/compatibility-policy/#tested-kubernetes-range).

### Managed clusters

EKS, GKE, AKS, k3s, RKE2 and OpenShift need no configuration. On managed
control planes, `/metrics` is usually forbidden (deprecated-API requests are
then not assessed) and control-plane pods are hidden (controller-manager and
scheduler skew are then not checked; kubelets and kube-proxy still are).
[Managed clusters](https://abd-ulbasit.github.io/upgradescope/guides/managed-clusters/).

## What the agent can touch

It reads with `get` and `list`, never `watch`, and writes only its own
`ClusterReadiness` object, its status and (by default) that object's CRD.
To read Helm releases it needs `get`/`list` on Secrets and ConfigMaps
cluster-wide, which RBAC cannot narrow by label. `rbac.helmSecretsNamespaces`
narrows it to a Role in each namespace that holds releases (still every
Secret and ConfigMap there, and releases elsewhere go unassessed);
`rbac.helmSecrets=false` removes it at the cost of Helm findings.
[Security model and RBAC](https://abd-ulbasit.github.io/upgradescope/operations/security-model-and-rbac/).

## Measured

| What | Measured | How |
|---|---|---|
| `scan` against a live kind cluster (Kubernetes 1.37) | median 0.49 s, p90 0.52 s | 30 runs on a ThinkPad (Linux, amd64), October 2026 audit at main `8a951dd` (#130) |
| `scan --files` on the demo's four rendered objects (`hack/demo/rendered`) | median 0.022 s, p90 0.022 s | 30 runs, Apple M1 Pro, main `9d0b161`, 2026-10-02 |
| Binary and archive, linux/amd64 (stripped binary, `.tar.gz` download) | 61.0 MiB binary, 18.6 MiB archive | GoReleaser v2.17.1 snapshot (`make release-check`) in CI's release-check job on GitHub's linux/amd64 runner, Go 1.26.9, main `f121698a` with #291, 2026-10-10; cross-compiled with `CGO_ENABLED=0`, so the build host does not change the size; the release check fails beyond 2% |

More, with sizes per platform: [Install](https://abd-ulbasit.github.io/upgradescope/operations/install/#sizes).

## Scale and cost

Measured on 4 October 2026 with the harness in `hack/bench/`: the agent on
main `735751d` (03:02 to 05:42 UTC) and at the commits of #226 and #228 each
row names (03:24 to 03:51, 06:01 to 06:09 and 06:55 to 07:03 UTC), the
server earlier (04:30 to 05:00 in UTC+5, which is 3 October in UTC) at main
`f195ba5` plus #71's work. The
cluster was a kind control plane (Kubernetes 1.37.0) on a ThinkPad with an
Intel Core i3-7100U (2 cores, 4 threads) and 7.3 GiB of RAM, filled by
[KWOK](https://kwok.sigs.k8s.io/) v0.8.0 with 2,000 fake nodes, 10,000 pods
(plus 4,000 DaemonSet pods), 6,000 ConfigMaps, 4,000 Deployments and 1,000
real `helm.sh/release.v1` Secrets; the agent ran on the same host. A MacBook
Pro (`MacBookPro18,3`, M1 Pro) drove the seeding. **The nodes are fake**: no
kubelet load, one kind apiserver, and generated objects, including the
Argo CD and Flux ones.
[Scale and cost](https://abd-ulbasit.github.io/upgradescope/operations/scale/)
has the tables, the simulation's limits and the open hotspots.

| What | Measured | How |
|---|---|---|
| A steady agent tick at 2,001 nodes, about 14,000 pods and 1,000 Helm releases, with the pod pass reused (the default since #228: the pods outside `kube-system` are listed every 3rd tick, `--pod-pass-every`, and add-ons are detected from that pass in between; an add-on changed after a pass, other than through Helm or a GitOps chart reference, shows its old version for at most 2 ticks, and a pass `--pod-pass-max-age` old, 1h, is not reused) | 20 API requests (31 on the tick that lists every pod; a cycle of 3 averages 23.7, computed), 27.8 MiB read (1.5 MiB on the wire), 2.4 s, 1.8 CPU-seconds; the full tick: 50.4 MiB (3.3 on the wire), 4.4 s, 3.4 CPU-seconds, peak heap 30.5 MiB; the reusing ticks' peak heap 36.4 MiB; the run's peak RSS 63.6 MiB (the process peak so far, not a figure per tick) | median of the 5 ticks that reused the pass and of the 2 that listed every pod after the first of 8, `make bench-agent` with `BENCH_TICKS=8`, at `0b6fec28` (10 October 2026, 02:56 to 03:12 UTC, lab B idle); the heap is the maximum. The host's load average was 8.5 to 11.9 (4 threads), so the full tick's 4.4 s and 3.4 CPU-seconds are not comparable with the 3.7 s and 3.0 of the next row, and say nothing about the pod-pass code; the requests, which load does not move, are the same. Later commits (`8ca7e786`, `81761afd`) leave the requests and, by estimate from the seed, the heap and RSS unchanged (not re-measured; Scale and cost says why) |
| A steady agent tick that lists every pod (`--pod-pass-every=1`) at the same size | 31 API requests since #228 (53 on main `735751d`; 4 more on a tick that asks API discovery again, at least hourly), 50.4 MiB read (3.1 MiB on the wire), 3.7 s, 3.0 CPU-seconds; peak heap 39.2 MiB, peak RSS 65.0 MiB. On main: 50 MiB read (3.4 MiB on the wire), 4.1 s, 3.0 CPU-seconds, peak live heap 28 MiB, peak RSS 56 MiB. A pod page's worst case is larger: 1,000 times the largest pod, which past about 60 KiB a pod after small ones would exceed the agent's `GOMEMLIMIT` (computed from the live heap measured on GitHub's linux/amd64 runner; see Scale and cost) | median of the 4 ticks after the first (the mean of the middle two; the heap is their maximum), `make bench-agent`, at `9810fb6` (06:55 to 07:03 UTC) and on main `735751d` |
| The first tick after the agent starts (reads each Helm release once, 8 at a time since #226) | 1,037 requests, 72.7 MiB, 42.4 s, 22.0 CPU-seconds at `9810fb6`, on a host whose load average rose to 15 (27.1 s and 22.8 at the draft `59d8561`, at a load of 7 to 12, where main took 36.1 s); at a 60 ms round trip 30.4 s (`06cdf7a`) and 29.6 s (`5da764e`). On main `735751d`: 1,053 requests, 73 MiB, 36.7 s, 23.5 CPU-seconds, and at a 60 ms round trip the Helm step's deadline after 67.6 s with 231 of 1,000 releases unread | the first of 5 ticks of each run (of 3 at 60 ms) |
| The same steady tick with 1,000 Argo CD Applications and 1,000 Flux HelmReleases (500 chartRefs) | 81 API requests since #248, 50 of them GitOps (the OCIRepositories listed, not a GET each), every chart resolved; 59.8 MiB, 7.5 s, 5.3 CPU-seconds (596 requests, 540 GitOps, 63 MiB, 12.8 s and 6.2 on main `735751d`) | median of 4 ticks, `BENCH_GITOPS=1`, `da90a86e`, 9 October 2026; a host at a load average of 20 to 28, so the wall time is a range (7.2 to 7.8 s; 11.0 to 18.1 s on `735751d`) |
| The first tick as a pod under the chart's limit, 1,000 Helm releases | 200m (the old default): 73 s, gave up at its Helm step with 132 releases unread (75.5 s and 43 unread at `da90a86e`, with #226 and #228; the next tick fetched only those 43 but used 9.0 CPU-seconds against a steady 2.9, #247); 500m: 48 s; 1 CPU (the default now): 35 s | one first tick each, `hack/bench/pod-sample.sh`, main `735751d` (before #226, which waits less but decodes no faster: at 1 CPU it saves at most the 13.4 s the GETs waited; computed); a steady tick took 21 s at 200m and 7 s at 1 CPU; the 200m re-run: `da90a86e`, 9 October 2026, CPU from the cgroup, requests from the audit log |
| Decoding the fill's 1,000 Helm release payloads (gzip, JSON, then the manifest), one at a time | 6.186 s of CPU, against 9.160 s before #285 parsed each manifest document once; the 50 large releases 2.538 s against 3.834 s. The findings and the heap bounds are the same | mean of 4 runs of each, alternating, `TestHelmDecodeCostByRelease` on the ThinkPad outside a CPU quota, main `4b1c2893` and `a14f737c` (10 October 2026, 11:34 to 11:37 UTC, host load average 1.5 to 2.7; lab B was not queried; two later commits that change production code, a decline of text with U+0085, U+2028 or U+2029 that the lab's payloads do not hold, and a refactor, were not re-measured); not a tick's CPU, and no pod ran it |
| The same steady tick before #71 (a GET per release every tick) | 1,061 requests, 90 MiB, 33 s, 23.5 CPU-seconds | median of 4 ticks, the first session's run (main `a3e72ea`), same harness with the Helm step given no cache |
| `serve` taking 200 clusters x 3 targets, all pushing at once | SQLite 56 new snapshots a second (p99 3.5 s); Postgres 17: 40 a second (p99 4.8 s); no failed push or retry | 200 pushers, `make bench-ingest`; 25 CPU-ms a snapshot on either |
| Storage per changed snapshot (3 evaluations) | at most about 100 KiB on SQLite (WAL and page overhead included), 19 KiB on Postgres | database growth over 200 new snapshots averaging 28 KiB |

Reproduce: `BENCH_RUN_ON=<ssh host of your lab> make bench-agent
KUBECONFIG=<lab kubeconfig>` (a disposable cluster; no other kubeconfig is
ever read) and `make bench-ingest`.

## How it compares

pluto, kubent and kubepug find deprecated APIs in manifests, Helm releases
and live objects, as one-shot CLIs; Nova finds outdated charts; EKS, GKE and
AKS check their own clusters. upgradescope adds authorship-based live
detection, cited add-on end of life, skew, a verdict that admits what it
could not see, and a continuous agent and fleet server you host. The
[comparison](https://abd-ulbasit.github.io/upgradescope/comparison/) says,
with sources, what each does that upgradescope does not.

## Project

[Contributing](CONTRIBUTING.md) · [Architecture](docs/architecture.md) ·
[REST API](https://abd-ulbasit.github.io/upgradescope/reference/api/) ([OpenAPI](api/openapi.yaml)) ·
[Changelog](CHANGELOG.md) · [Security](SECURITY.md) ·
[Compatibility policy](https://abd-ulbasit.github.io/upgradescope/compatibility-policy/)

The author interned at chkk.io, which sells in this category. upgradescope
is clean-room: no proprietary code, data, schemas or documents were used,
and every knowledge-base entry is generated from upstream source or carries
a public citation, checked in CI. Most of the code was written by coding
agents from specs and plans the author wrote and reviewed.

Apache-2.0.
