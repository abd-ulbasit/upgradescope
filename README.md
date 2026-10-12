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
   range, every claim with a public citation (Ingress NGINX, retired in
   March 2026, is a blocker).
2. **Custom resources at CRD versions no longer served**, and
   `status.storedVersions` entries that block the CRD update dropping one.
3. **Stored Helm release manifests and chart `kubeVersion`**: what the
   installed revision would replay at the target, and charts that exclude it.
4. **Pull-request and CI gating**: `scan --files` on rendered manifests, with
   SARIF, JUnit and GitLab Code Quality output; the GitHub Action needs
   nothing installed.

Removed and deprecated APIs, version skew and removed in-tree volume plugins
are checked too.

The answer is a verdict (`ready`, `blocked`, or `unknown` when a required
check could not run), cited findings and an exit code.

**Documentation: https://abd-ulbasit.github.io/upgradescope/** ·
every public claim, with the test that proves it: [claims ledger](docs/claims.md)

![upgradescope scanning rendered manifests](docs/img/demo.gif)

## Install

```sh
brew install abd-ulbasit/tap/upgradescope
go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@latest
helm install upgradescope oci://ghcr.io/abd-ulbasit/charts/upgradescope --version 0.2.2 -n upgradescope --create-namespace
```

In CI: `uses: abd-ulbasit/upgradescope@v0.2.2`, on
[GitHub Marketplace](https://github.com/marketplace/actions/upgradescope-gate)
([action/README.md](action/README.md)). Archives, packages, the signed image
and their verification:
[Install](https://abd-ulbasit.github.io/upgradescope/operations/install/).

Verifying a download:

```sh
VERSION=v0.2.2   # the release you downloaded
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

Scan rendered manifests, or the cluster your kubeconfig points at:

```sh
upgradescope scan --files rendered/ --target 1.37
upgradescope scan --target 1.37
```

Exit code 0 means the gate passed, 2 that it failed (a blocker, or
`unknown`), 1 an error. `--fail-on warning|never` moves the threshold;
`--allow-incomplete` gates on findings alone when a required check could not
run. [CLI in two minutes](https://abd-ulbasit.github.io/upgradescope/getting-started/cli/).

Gate pull requests with the Action (pin a release tag or its SHA):

```yaml
      - run: helm template my-release ./chart --output-dir rendered
      - uses: abd-ulbasit/upgradescope@v0.2.2
        id: gate
        with: {path: rendered, target: "1.37", version: v0.2.2}
      - uses: github/codeql-action/upload-sarif@v4
        # also when the gate failed; not on a fork PR, whose token is read-only
        if: ${{ !cancelled() && steps.gate.outputs.sarif-file != '' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}
        with: {sarif_file: "${{ steps.gate.outputs.sarif-file }}"}
```

[CI gate](https://abd-ulbasit.github.io/upgradescope/getting-started/ci-gate/)
covers fork and Dependabot pull requests and where the alerts appear;
GitLab CI, Jenkins and Azure Pipelines have templates in [`ci/`](ci/)
([Other CI systems](https://abd-ulbasit.github.io/upgradescope/guides/other-ci/)).

In the cluster, the chart's agent keeps a `ClusterReadiness` object current:

```sh
kubectl get ucr        # NAME  TARGET  SCORE  READY  BLOCKERS  LASTEVALUATED  AGE
```

**What the agent can touch.** It reads with `get` and `list`, never
`watch`, and writes only ClusterReadiness objects, their status and that
CRD; `create` on `clusterreadinesses` cannot be limited by name, and it
never deletes one. Helm releases need Secrets and ConfigMaps read
cluster-wide, or per namespace with `rbac.helmSecretsNamespaces`, or not at
all with `rbac.helmSecrets=false`; the volume checks list
`persistentvolumes` and `storageclasses`.
[Security model and RBAC](https://abd-ulbasit.github.io/upgradescope/operations/security-model-and-rbac/).

## What it checks

| Check | A blocker when | Read from |
|---|---|---|
| [Add-ons](https://abd-ulbasit.github.io/upgradescope/concepts/addon-registry/) | past end of life, or the installed line excludes the target | images, Helm releases, chart labels, `IngressClass` |
| CRD versions | custom resources at a version their CRD no longer serves | CRDs and their objects |
| Helm charts | `kubeVersion` excludes the target, or a stored release uses a removed API | Helm releases; Argo CD and Flux charts (opt-in) |
| [Removed APIs](https://abd-ulbasit.github.io/upgradescope/concepts/api-usage-detection/) | an object is still written through an API the target removed | `managedFields`, then the last-applied annotation; manifests |
| Deprecated API requests | the apiserver was asked for an API the target removed | `apiserver_requested_deprecated_apis` |
| Version skew | kubelets or kube-proxy outside the policy at the target | node and control-plane versions |
| [In-tree volume plugins](https://abd-ulbasit.github.io/upgradescope/concepts/volume-plugins/) | a removed plugin (`glusterfs`, `gitRepo`) is still named | pods, PersistentVolumes, StorageClasses |
| Managed-provider support | the EKS, GKE or AKS minor is past standard support | the provider's calendar |

Where it stops:

- Field-level removals other than in-tree volume plugins
  (`Service.spec.externalIPs`, the seccomp alpha annotations) are not
  checked, and a `READY` with a score of 100 does not cover them; the table
  and Markdown reports say so.
- The deprecated-API metric shows that a client asked, not which client,
  and only for the replica that answered; managed control planes often
  forbid it.
- Add-ons outside the registry are not judged (the report lists the
  unrecognised images); Helm's sql driver is not read; kubectl client skew
  is not checked (only audit logs reveal clients).
- The knowledge base is compiled into the binary; `upgradescope version`
  prints the newest minor it covers, and a target past it is `unknown`.

Tested against Kubernetes 1.24 to the newest minor
([range](https://abd-ulbasit.github.io/upgradescope/compatibility-policy/#tested-kubernetes-range));
EKS, GKE, AKS, k3s, RKE2 and OpenShift need no configuration
([managed clusters](https://abd-ulbasit.github.io/upgradescope/guides/managed-clusters/)).
[Verdict and score](https://abd-ulbasit.github.io/upgradescope/concepts/verdict-and-score/)
has every category and rule.

## Also included

- **In-cluster agent**: re-evaluates every 10 minutes and keeps a standard
  `Ready` condition for `kubectl wait`, alerts, GitOps health checks and
  policy engines ([examples](examples/)).
  [In-cluster agent](https://abd-ulbasit.github.io/upgradescope/getting-started/in-cluster/).
- **Self-hosted server**: agents push to one `upgradescope serve` for
  history, a fleet view, team rollups, a server-side CI gate, auditor
  exports and an embedded dashboard, on SQLite or Postgres.
  [Fleet server](https://abd-ulbasit.github.io/upgradescope/getting-started/fleet/) ·
  [dashboard](https://abd-ulbasit.github.io/upgradescope/guides/dashboard/).
- **Notifications** to Slack or an optionally signed webhook when readiness
  changes. [Notifications](https://abd-ulbasit.github.io/upgradescope/operations/#notifications).
- **AI assistants**: `upgradescope mcp` gives Claude Code
  (`claude mcp add upgradescope -- upgradescope mcp`) or any MCP client
  read-only tools over these reports.
  [MCP](https://abd-ulbasit.github.io/upgradescope/getting-started/mcp/).
- **Outputs**: table, JSON, SARIF, Markdown, JUnit, GitLab Code Quality, and
  a 0–100 score for trends.

## Measured

| What | Measured | How |
|---|---|---|
| `scan` against a live kind cluster (Kubernetes 1.37) | median 0.49 s, p90 0.52 s | 30 runs on a ThinkPad (Linux, amd64), October 2026 audit at main `8a951dd` (#130) |
| `scan --files` on the demo's four rendered objects (`hack/demo/rendered`) | median 0.022 s, p90 0.022 s | 30 runs, Apple M1 Pro, main `9d0b161`, 2026-10-02 |
| A steady agent tick at 2,001 fake (KWOK) nodes, about 14,000 pods and 1,000 Helm releases, reusing the pod pass (the default) | 20 API requests, 27.8 MiB read (1.5 MiB on the wire), 2.4 s; 31 requests on the tick that lists every pod | medians of the 5 ticks that reused the pass (31: the 2 that listed every pod), after the first of 8, `make bench-agent` at `0b6fec28` on lab A, 10 October 2026; #362 later added two volume lists (22 and 33, computed) |
| Binary and archive, linux/amd64 (stripped binary, `.tar.gz` download) | 61.5 MiB binary, 18.7 MiB archive | the published v0.2.0 assets (commit `62603159`, GoReleaser v2.17.1, Go 1.26.9), 2026-10-11; the release check fails beyond 2% |

[Scale and cost](https://abd-ulbasit.github.io/upgradescope/operations/scale/)
has the first tick, GitOps objects, CPU limits, the server's ingest and how
to reproduce them;
[Install](https://abd-ulbasit.github.io/upgradescope/operations/install/#sizes)
has the sizes per platform.

## How it compares

pluto, kubent and kubepug find deprecated APIs as one-shot CLIs; Nova finds
outdated charts; EKS, GKE and AKS check their own clusters.
[kube-upgrade-check](https://github.com/runtimez-com/kube-upgrade-check)
(krew plugin `upgrade-check`) covers similar live-cluster ground, also from
`managedFields` and the deprecated-API metric, and adds control-plane and
kubelet config checks, release-note checks and upgrade preflight
(drain-blocking PodDisruptionBudgets, webhooks with a `Fail` policy) that
upgradescope does not do yet
([#397](https://github.com/abd-ulbasit/upgradescope/issues/397)).
upgradescope adds cited add-on end of life, CRD version checks, stored Helm
release replay and a gate on rendered manifests. The
[comparison](https://abd-ulbasit.github.io/upgradescope/comparison/) says,
with sources, what each does that upgradescope does not.

**Use something else when** you need upgrade preflight, or control-plane
flag, kubelet config and release-note checks on a self-managed cluster:
kube-upgrade-check does those today.

## Project

[Contributing](CONTRIBUTING.md) · [Governance](GOVERNANCE.md) ·
[Maintainers](MAINTAINERS.md) · [Adopters](ADOPTERS.md) ·
[Roadmap](ROADMAP.md) · [Security](SECURITY.md) ·
[Claims ledger](docs/claims.md) · [Code of Conduct](CODE_OF_CONDUCT.md) ·
[Changelog](CHANGELOG.md) ·
[REST API](https://abd-ulbasit.github.io/upgradescope/reference/api/) ([OpenAPI](api/openapi.yaml)) ·
[Compatibility policy](https://abd-ulbasit.github.io/upgradescope/compatibility-policy/)

The author interned at chkk.io, which sells in this category. upgradescope
is clean-room: no proprietary code, data, schemas or documents were used,
and every knowledge-base entry is generated from upstream source or carries
a public citation, checked in CI. Most of the code was written by coding
agents (see the `Co-Authored-By` trailers in the git history), working from
specs and plans drafted with them and approved by the author; the planning
notes are not published, and the design decisions are in the
[architecture guide](docs/architecture.md).

Apache-2.0.
