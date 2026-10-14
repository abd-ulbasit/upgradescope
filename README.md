# upgradescope

[![CI](https://github.com/abd-ulbasit/upgradescope/actions/workflows/ci.yml/badge.svg)](https://github.com/abd-ulbasit/upgradescope/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/abd-ulbasit/upgradescope)](https://github.com/abd-ulbasit/upgradescope/releases)
[![Docs](https://img.shields.io/badge/docs-site-blue)](https://abd-ulbasit.github.io/upgradescope/)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/abd-ulbasit/upgradescope/badge)](https://scorecard.dev/viewer/?uri=github.com/abd-ulbasit/upgradescope)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/upgradescope)](https://artifacthub.io/packages/search?repo=upgradescope)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

upgradescope tells you, before you upgrade Kubernetes, what will break in
one cluster, a fleet, or the manifests in a pull request:

1. **Add-on end of life and compatibility**, from cited data for 34 add-ons:
   the end-of-life date and, where upstream publishes one, the Kubernetes
   range (Ingress NGINX, retired in March 2026, is a blocker).
2. **Custom resources at CRD versions no longer served**, and
   `status.storedVersions` entries that block the CRD update dropping one.
3. **Stored Helm release manifests and chart `kubeVersion`**: what the
   installed revision would replay at the target, and charts that exclude it.
4. **Pull-request and CI gating**: `scan --files` on rendered manifests, with
   SARIF, JUnit and GitLab Code Quality output.

Removed and deprecated APIs, version skew and removed in-tree volume plugins
are checked too. The answer is a verdict (`ready`, `blocked`, or `unknown`
when a required check could not run), cited findings and an exit code.

**Documentation: https://abd-ulbasit.github.io/upgradescope/** ·
every public claim, with the test that proves it: [claims ledger](docs/claims.md)

![upgradescope scanning rendered manifests](docs/img/demo.gif)

## Install

```sh
brew install abd-ulbasit/tap/upgradescope
go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@latest
helm install upgradescope oci://ghcr.io/abd-ulbasit/charts/upgradescope --version 0.3.0 -n upgradescope --create-namespace
```

In CI: `uses: abd-ulbasit/upgradescope@v0.3.0`
([Marketplace](https://github.com/marketplace/actions/upgradescope-gate),
[action/README.md](action/README.md)). Every channel and its verification:
[Install](https://abd-ulbasit.github.io/upgradescope/operations/install/).

Verifying a download:

```sh
VERSION=v0.3.0   # the release you downloaded
base=https://github.com/abd-ulbasit/upgradescope/releases/download/$VERSION
curl -fsSLO "$base/upgradescope_linux_amd64.tar.gz"
curl -fsSLO "$base/checksums.txt"
curl -fsSLO "$base/checksums.txt.sigstore.json"
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/abd-ulbasit/upgradescope/.github/workflows/release.yml@refs/tags/$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 -c --ignore-missing checksums.txt
```

## Quickstart

Scan rendered manifests, or your kubeconfig's cluster:

```sh
upgradescope scan --files rendered/ --target 1.37
upgradescope scan --target 1.37
```

Exit code 0: the gate passed; 2: it failed (a blocker, or `unknown`); 1: an
error. `--fail-on` moves the threshold; `--allow-incomplete` gates on
findings alone. [CLI in two minutes](https://abd-ulbasit.github.io/upgradescope/getting-started/cli/).

Gate pull requests with the Action:

```yaml
      - run: helm template my-release ./chart --output-dir rendered
      - uses: abd-ulbasit/upgradescope@v0.3.0
        id: gate
        with: {path: rendered, target: "1.37", version: v0.3.0}
      - uses: github/codeql-action/upload-sarif@v4
        # also after a failed gate; not on fork PRs (read-only token)
        if: ${{ !cancelled() && steps.gate.outputs.sarif-file != '' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}
        with: {sarif_file: "${{ steps.gate.outputs.sarif-file }}"}
```

[CI gate](https://abd-ulbasit.github.io/upgradescope/getting-started/ci-gate/)
covers fork and Dependabot pull requests; [`ci/`](ci/) has GitLab CI,
Jenkins and Azure Pipelines templates
([Other CI systems](https://abd-ulbasit.github.io/upgradescope/guides/other-ci/)).

In the cluster, the chart's agent keeps a `ClusterReadiness` object current
(`kubectl get ucr`).

**What the agent can touch.** It reads with `get` and `list`, never
`watch`, and writes only ClusterReadiness objects, their status and that
CRD (`create` on `clusterreadinesses` cannot be limited by name; it never
deletes). Helm releases need Secrets and ConfigMaps read cluster-wide, per
namespace with `rbac.helmSecretsNamespaces`, or not at all with
`rbac.helmSecrets=false`; the volume checks list `persistentvolumes` and
`storageclasses`.
[Security model and RBAC](https://abd-ulbasit.github.io/upgradescope/operations/security-model-and-rbac/).

## What it checks

| Check | A blocker when | Read from |
|---|---|---|
| [Add-ons](https://abd-ulbasit.github.io/upgradescope/concepts/addon-registry/) | past end of life, or the installed line excludes the target | images, Helm releases, chart labels, `IngressClass` |
| CRD versions | objects at a version their CRD no longer serves | CRDs and their objects |
| Helm charts | `kubeVersion` excludes the target, or a stored release uses a removed API | Helm releases; Argo CD and Flux (opt-in) |
| [Removed APIs](https://abd-ulbasit.github.io/upgradescope/concepts/api-usage-detection/) | an object is written through a removed API | `managedFields`, last-applied annotation, manifests |
| Deprecated API requests | the apiserver was asked for a removed API | `apiserver_requested_deprecated_apis` |
| Version skew | kubelets or kube-proxy outside the policy | node and control-plane versions |
| [In-tree volume plugins](https://abd-ulbasit.github.io/upgradescope/concepts/volume-plugins/) | a removed plugin (`glusterfs`, `gitRepo`) is named | pods, PersistentVolumes, StorageClasses |
| Managed-provider support | the EKS, GKE or AKS minor is past standard support | the provider's calendar |

Where it stops:

- Field-level removals other than in-tree volume plugins
  (`Service.spec.externalIPs`) are not checked; the table and Markdown
  reports say so.
- The deprecated-API metric shows that a client asked, not which, and only
  on the replica that answered.
- Add-ons outside the registry are not judged (their images are listed);
  Helm's sql driver is not read; kubectl client skew is not checked (only
  audit logs reveal clients).
- The knowledge base is compiled in: `upgradescope version` prints its
  newest minor; a later target is `unknown`.

Tested against Kubernetes 1.24 to the newest minor
([range](https://abd-ulbasit.github.io/upgradescope/compatibility-policy/#tested-kubernetes-range));
EKS, GKE, AKS, k3s, RKE2 and OpenShift need no configuration
([managed clusters](https://abd-ulbasit.github.io/upgradescope/guides/managed-clusters/)).
[Verdict and score](https://abd-ulbasit.github.io/upgradescope/concepts/verdict-and-score/)
has every rule.

## Also included

- **In-cluster agent**: re-evaluates every 10 minutes and keeps a `Ready`
  condition for `kubectl wait` and GitOps health checks.
  [In-cluster agent](https://abd-ulbasit.github.io/upgradescope/getting-started/in-cluster/) ·
  [examples](examples/).
- **Fleet server**: `upgradescope serve` for history, team rollups, a
  server-side gate, auditor exports and a dashboard, on SQLite or Postgres.
  [Fleet server](https://abd-ulbasit.github.io/upgradescope/getting-started/fleet/) ·
  [dashboard](https://abd-ulbasit.github.io/upgradescope/guides/dashboard/).
- **Notifications** to Slack or an optionally signed webhook.
  [Notifications](https://abd-ulbasit.github.io/upgradescope/operations/#notifications).
- **MCP**: `upgradescope mcp` gives AI assistants read-only tools over the
  reports. [MCP](https://abd-ulbasit.github.io/upgradescope/getting-started/mcp/).
- **Outputs**: table, JSON, SARIF, Markdown, JUnit, GitLab Code Quality, and
  a 0–100 score.

## Measured

| What | Measured | How |
|---|---|---|
| `scan`, a live kind cluster (Kubernetes 1.37) | median 0.49 s, p90 0.52 s | 30 runs, ThinkPad (linux/amd64), main `8a951dd` (#130), October 2026 |
| `scan --files`, the demo's four objects (`hack/demo/rendered`) | median 0.022 s, p90 0.022 s | 30 runs, Apple M1 Pro, main `9d0b161`, 2026-10-02 |
| A steady agent tick: 2,001 KWOK nodes, about 14,000 pods, 1,000 Helm releases | 20 API requests, 27.8 MiB read, 2.4 s; 31 requests when it lists every pod | medians, `make bench-agent` at `0b6fec28`, lab A, 2026-10-10; #362 added two lists since (22 and 33, computed) |
| Binary and archive, linux/amd64 (stripped binary, `.tar.gz`) | 61.5 MiB binary, 18.7 MiB archive | the published v0.2.0 assets (`62603159`), 2026-10-11; the release check fails beyond 2% |

[Scale and cost](https://abd-ulbasit.github.io/upgradescope/operations/scale/#the-readmes-measured-table)
has each row in full and how to reproduce it;
[Install](https://abd-ulbasit.github.io/upgradescope/operations/install/#sizes)
has every platform's size.

## How it compares

pluto, kubent and kubepug find deprecated APIs; Nova, outdated charts; EKS,
GKE and AKS check their own clusters.
[kube-upgrade-check](https://github.com/runtimez-com/kube-upgrade-check)
covers similar live-cluster ground, also from `managedFields` and the
deprecated-API metric. The
[comparison](https://abd-ulbasit.github.io/upgradescope/comparison/#kube-upgrade-check)
has the detail and the sources.

**Use something else when** you need upgrade preflight (drain-blocking
PodDisruptionBudgets, webhooks with a `Fail` policy; not checked until
[#397](https://github.com/abd-ulbasit/upgradescope/issues/397)), or
control-plane flag, kubelet config and release-note checks on a
self-managed cluster: kube-upgrade-check does those today.

## Project

[Contributing](CONTRIBUTING.md) · [Governance](GOVERNANCE.md) ·
[Maintainers](MAINTAINERS.md) · [Adopters](ADOPTERS.md) ·
[Roadmap](ROADMAP.md) · [Security](SECURITY.md) ·
[Claims ledger](docs/claims.md) · [Code of Conduct](CODE_OF_CONDUCT.md) ·
[Changelog](CHANGELOG.md) ·
[REST API](https://abd-ulbasit.github.io/upgradescope/reference/api/) ([OpenAPI](api/openapi.yaml)) ·
[Compatibility policy](https://abd-ulbasit.github.io/upgradescope/compatibility-policy/)

**Status:** maintenance mode since 15 October 2026: data refreshes and fixes,
no new features ([roadmap](ROADMAP.md)).

The author interned at chkk.io, which sells in this category. upgradescope
is clean-room: no proprietary code, data, schemas or documents were used,
and every knowledge-base entry is generated from upstream source or carries
a public citation, checked in CI. Most of the code was written by coding
agents (see the `Co-Authored-By` trailers in the git history), working from
specs and plans drafted with them and approved by the author; the planning
notes are not published, and the design decisions are in the
[architecture guide](docs/architecture.md).

Apache-2.0.
