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
outside the upstream policy, and Helm charts that exclude the target. The
answer is a verdict (`ready`, `blocked` or `unknown` when a required check
could not run), a 0–100 score and cited findings, as a table, JSON, SARIF,
Markdown, JUnit or GitLab Code Quality, an exit code for CI, and a
`ClusterReadiness` object an in-cluster agent keeps current. A self-hosted server adds history, a fleet view, team
rollups and auditor exports. One Apache-2.0 binary. `scan` only reads; the
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
`--allow-incomplete` gates on findings alone when a required check could not
run, for example a target newer than the knowledge base knows.
[CLI in two minutes](https://abd-ulbasit.github.io/upgradescope/getting-started/cli/).

**Gate pull requests** with the GitHub Action (pin it to a release tag or
its SHA; the floating `v0` tag moves with every release):

```yaml
      - run: helm template my-release ./chart --output-dir rendered
      - uses: abd-ulbasit/upgradescope@v0.2.0
        id: gate
        with: {path: rendered, target: "1.37", version: v0.2.0}
      - uses: github/codeql-action/upload-sarif@v4
        if: ${{ !cancelled() && steps.gate.outputs.sarif-file != '' }}
        with: {sarif_file: "${{ steps.gate.outputs.sarif-file }}"}
```

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
object with a standard `Ready` condition, for `kubectl wait`, alerts and
GitOps health checks. Until v0.2.0 is published, the chart's default image
does not exist: build one from the clone and set `image.repository` and
`image.tag` ([#127](https://github.com/abd-ulbasit/upgradescope/issues/127)).
[In-cluster agent](https://abd-ulbasit.github.io/upgradescope/getting-started/in-cluster/) ·
[Fleet server](https://abd-ulbasit.github.io/upgradescope/getting-started/fleet/).

## What it checks, and where it stops

- **Removed and deprecated APIs**, in manifests and in live objects. Live
  objects count when someone still *writes* them through the deprecated
  version (from `managedFields`, then the last-applied annotation), not
  merely because the apiserver still serves that version, which would flag
  every cluster. Objects with neither record are missed.
  [How](https://abd-ulbasit.github.io/upgradescope/concepts/api-usage-detection/).
- **Deprecated API requests**, from the apiserver's
  `apiserver_requested_deprecated_apis` metric: that a client asked, not
  which client, and only for the replica that answered. Managed control
  planes often forbid it; the report then says so.
- **Add-ons past end of life** and their Kubernetes compatibility, from a
  registry of 20 add-ons in which every claim carries a citation (Ingress
  NGINX, retired in March 2026, is a blocker). An add-on is found by its
  container images, its Helm release, its `helm.sh/chart` or
  `app.kubernetes.io/*` pod labels, or an Ingress NGINX `IngressClass`; in
  rendered manifests, by the images and labels of workload pod templates.
  Add-ons outside the registry are not judged; the report lists the images
  no matcher recognised.
  [Registry](https://abd-ulbasit.github.io/upgradescope/concepts/addon-registry/).
- **Version skew** of kubelets, kube-proxy, controller-manager, scheduler
  and HA apiservers; not `kubectl` clients, which only audit logs reveal.
- **Helm charts** whose `kubeVersion` excludes the target, and stored
  release manifests that use removed APIs.

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
cluster-wide, which RBAC cannot narrow; `rbac.helmSecrets=false` removes that
at the cost of Helm findings.
[Security model and RBAC](https://abd-ulbasit.github.io/upgradescope/operations/security-model-and-rbac/).

## Measured

| What | Measured | How |
|---|---|---|
| `scan` against a live kind cluster (Kubernetes 1.37) | median 0.49 s, p90 0.52 s | 30 runs on a ThinkPad (Linux, amd64), October 2026 audit at main `8a951dd` (#130) |
| `scan --files` on the demo's four rendered objects (`hack/demo/rendered`) | median 0.022 s, p90 0.022 s | 30 runs, Apple M1 Pro, main `9d0b161`, 2026-10-02 |
| Binary, linux/amd64 (`-trimpath -ldflags "-s -w"`, as released) | 57.4 MiB (16.8 MiB gzipped) | Go 1.26.8, main `9d0b161`, 2026-10-02 |

More, with sizes per platform: [Install](https://abd-ulbasit.github.io/upgradescope/operations/install/#sizes).

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
