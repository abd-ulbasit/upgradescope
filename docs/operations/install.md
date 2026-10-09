# Install

Every channel ships the same binary, built from the tagged commit by the
release workflow, with the dashboard and the knowledge base embedded.

!!! note "Which release has what"
    v0.2.0 is the first release built, signed and published by CI. It adds
    Windows archives, deb/rpm/apk packages, cosign signatures, SBOMs, the
    signed image and the OCI Helm chart, and the Homebrew tap starts with
    it. v0.1.0 and v0.1.1 published linux and darwin archives with a
    `checksums.txt`, unsigned. The rows below say where a channel starts
    with v0.2.0.

| Channel | Command | Since |
|---|---|---|
| Release archive | download from [Releases](https://github.com/abd-ulbasit/upgradescope/releases), verify, extract | v0.1.1 (signed from v0.2.0) |
| Homebrew | `brew install abd-ulbasit/tap/upgradescope` | v0.2.0 |
| deb / rpm / apk | the packages attached to the release | v0.2.0 |
| krew | `kubectl krew install upgradescope` | once the plugin is in krew-index |
| Go | `go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@<version>` | any |
| Container image | `ghcr.io/abd-ulbasit/upgradescope:<version>` (linux/amd64, linux/arm64) | v0.2.0, signed |
| Helm chart | `oci://ghcr.io/abd-ulbasit/charts/upgradescope` | v0.2.0, signed; before that, from a clone |

## Release archives

Archives are named `upgradescope_<os>_<arch>.tar.gz` (`.zip` on Windows)
for linux, darwin and windows on amd64 and arm64. Each holds the binary,
shell completions, man pages and the license notices.

```sh
VERSION=v0.2.0
base=https://github.com/abd-ulbasit/upgradescope/releases/download/$VERSION
curl -fsSLO "$base/upgradescope_linux_amd64.tar.gz"
curl -fsSLO "$base/checksums.txt"
sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 -c --ignore-missing checksums.txt
tar -xzf upgradescope_linux_amd64.tar.gz
sudo install upgradescope /usr/local/bin/
```

### Verify a download

From v0.2.0, `checksums.txt` is signed keylessly by the repository's release
workflow ([cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
bundle `checksums.txt.sigstore.json`). Verify the signature, then the
archive against the checksums:

```sh
curl -fsSLO "$base/checksums.txt.sigstore.json"
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/abd-ulbasit/upgradescope/.github/workflows/release.yml@refs/tags/$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 -c --ignore-missing checksums.txt
```

Each archive also has an SPDX SBOM (`<archive>.sbom.json`) and a GitHub
build-provenance attestation (`gh attestation verify <archive> --repo
abd-ulbasit/upgradescope`).

The deb, rpm and apk packages are covered by the same `checksums.txt`, under
the names GitHub serves them with, so download them by those names and the
command above checks them too. (A pre-release's package names use `.` where
nfpm's own would use `~`: `upgradescope_0.2.0.rc.2_amd64.deb`. `release-check`
fails any asset name GitHub would rewrite.) `--ignore-missing` skips a file
checksums.txt does not list, so confirm the output names every package you
downloaded with `OK`.

## Homebrew

```sh
brew install abd-ulbasit/tap/upgradescope
```

Available from v0.2.0, for stable releases only: the tap skips
pre-releases (`-rc.N`), and its release notes do not offer it. The tap pulls
each stable release on a schedule, so a new release reaches it within about
six hours. Its formula is rendered only from releases whose `checksums.txt`
verifies against the release workflow's signature.

## Go

```sh
go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@latest
```

The dashboard is committed in the module, so this binary serves it too,
and `upgradescope version` reports the module version and commit (CI checks
both: `make go-install-check`). What it lacks is the registry date stamp
(`registry date: unknown`), and it is built with your toolchain rather than
the release's.

## Container image

```sh
docker run --rm ghcr.io/abd-ulbasit/upgradescope:0.2.0 version
```

A distroless, non-root (UID 65532) image with only the binary and its
license notices in `/licenses/`, for linux/amd64 and linux/arm64. Verify
the signature:

```sh
cosign verify ghcr.io/abd-ulbasit/upgradescope:0.2.0 \
  --certificate-identity-regexp '^https://github.com/abd-ulbasit/upgradescope/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Each platform image is signed too and verifies on its own, by digest.

## Helm chart

```sh
helm install upgradescope oci://ghcr.io/abd-ulbasit/charts/upgradescope \
  --version 0.2.0 -n upgradescope --create-namespace
```

The published chart pins the image of its release by digest. It is signed
with cosign (`cosign verify ghcr.io/abd-ulbasit/charts/upgradescope:0.2.0`
with the flags above). Before v0.2.0 is published, install from a clone of
the repository: `helm install upgradescope deploy/chart ...`, with
`image.repository` and `image.tag` set to an image built from the clone
(`make docker-build`) when you run unreleased changes: a clone's chart
defaults to the image of the release it was cut for.

Upgrade it with `helm upgrade --reset-then-reuse-values` (Helm 3.14 or
later), never `--reuse-values`, which keeps the old chart's defaults, the
pinned image digest among them: [Upgrade](upgrade.md#the-chart).

What the chart installs, and the agent-only, combined and fleet-hub setups:
[In-cluster agent](../getting-started/in-cluster.md), the
[chart README](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/chart/README.md)
and [Helm values](../reference/helm-values.md).

### Sizing the agent

The agent's defaults are 50m CPU and 64Mi requested, with limits of 1 CPU and
256Mi (`agent.resources`). The CPU limit is a quota, not a scheduling reservation (a namespace
`ResourceQuota` on `limits.cpu` still counts it), and
the first tick after every start reads each Helm release once: measured
against 2,000 fake nodes and 1,000 releases (1,001 with the chart's own),
the whole first tick took about 23 CPU-seconds, 35 s at 1
CPU and 48 s at 500m, both finishing the Helm step (which had 59 s in those
runs), and did not finish its Helm step at 200m, the chart's default
through v0.2.0-rc.2 (it left 132 releases unread, and the report's
`notAssessed` said so). If you set a lower limit on a cluster with many
releases, expect the first ticks to report `helm (partial)` until the cache
fills. At 1 CPU the Helm step took about 28 s of the 35, and since the tick
reserve (#238) the step has at most 54 s at the default interval, so above
about 1,900 releases (arithmetic, not measured) give the agent more than
1 CPU or a longer `agent.interval`. With `rbac.gitops.*` on, each tick also lists Argo
CD Applications and Flux HelmReleases and reads their OCIRepositories: 540
requests and 3 more CPU-seconds for 1,000 of each. These reads share the
Helm step's 54 s, so the ceiling is lower (about 1,600 releases by the
same arithmetic, taking the 8.7 s the GitOps reads added to a tick with no
quota).

Memory: the agent's peak RSS was 66 to 71 MiB as a pod at that size (main
`735751d`, before #228's larger pod pages, which raised the benchmark's peak
RSS by about 8 MiB). A pod page holds up to 1,000 pods, so 1,000 pods of about 70 KiB each, after a
page of small ones, would take the agent past its `GOMEMLIMIT` at the
256Mi limit (computed, see
[the tick after #226 and #228](scale.md#the-tick-after-226-and-228)). Raise
`agent.resources.limits.memory` on a cluster whose pods are that large, as
Argo Workflows pods, which carry their template, can be. The measurements,
and their limits, are in [Scale and cost](scale.md#cpu-and-the-chart-limit).

## Sizes

What a release ships, from a GoReleaser v2.17.1 snapshot (`make
release-check`, the build the release workflow runs) of `b507114` (with
the MCP server, #76), with Go 1.26.8, on 2026-10-03, on an Apple M1 Pro.
The binary is stripped
(`CGO_ENABLED=0 -trimpath -ldflags "-s -w"`); the archive is the download,
with the licenses, README, completions and man pages beside the binary.
Every platform is cross-compiled without cgo, so the build host does not
change a size; the Go version and the commit do. 1 MiB is 1,048,576 bytes.

| Platform | Binary | Archive |
|---|---|---|
| linux/amd64 | 59.9 MiB | 18.1 MiB |
| linux/arm64 | 56.3 MiB | 16.3 MiB |
| darwin/amd64 | 60.9 MiB | 18.4 MiB |
| darwin/arm64 | 57.7 MiB | 17.0 MiB |
| windows/amd64 | 61.0 MiB | 18.5 MiB |
| windows/arm64 | 56.5 MiB | 16.3 MiB |

Archives are `.tar.gz`, `.zip` on Windows. The release check
(`hack/check-doc-sizes.sh`, after the snapshot) fails when a size here or
in the README differs from the build by more than 2%, so these are re-measured
before a release ships a different size. On pull requests CI runs that
check only when they touch release inputs, so a code change that grows the
binary first fails a dispatched CI run or the release's own
release-check (scheduled runs skip it): run `make release-check`
(no Docker engine: `GORELEASER_SKIP=publish,sign,sbom,docker`) before
tagging, and update both pages when it fails.

The image adds the distroless base, whose layers are 0.7 MB compressed
(`gcr.io/distroless/static-debian12:nonroot`, as pinned in
`Dockerfile.release`), and about 0.3 MB of license notices, to the binary.
The size of the published v0.2.0 image will be measured from the registry
once it exists.
