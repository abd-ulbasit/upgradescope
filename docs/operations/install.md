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
sha256sum --ignore-missing -c checksums.txt
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
sha256sum --ignore-missing -c checksums.txt
```

Each archive also has an SPDX SBOM (`<archive>.sbom.json`) and a GitHub
build-provenance attestation (`gh attestation verify <archive> --repo
abd-ulbasit/upgradescope`).

## Homebrew

```sh
brew install abd-ulbasit/tap/upgradescope
```

Available from v0.2.0. The tap's formula is rendered only from releases
whose `checksums.txt` verifies against the release workflow's signature.

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
the repository: `helm install upgradescope deploy/chart ...`.

What the chart installs, and the agent-only, combined and fleet-hub setups:
[In-cluster agent](../getting-started/in-cluster.md), the
[chart README](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/chart/README.md)
and [Helm values](../reference/helm-values.md).

## Sizes

Measured 2026-10-02 on an Apple M1 Pro with Go 1.26.8, at commit 6516b61
(after v0.1.1), building as the release does (`CGO_ENABLED=0 -trimpath
-ldflags "-s -w"`):

| Binary | Stripped | Unstripped | gzip -9 |
|---|---|---|---|
| linux/amd64 | 57.3 MiB | 81.6 MiB | 16.8 MiB |
| linux/arm64 | 53.9 MiB | 77.4 MiB | 15.0 MiB |
| darwin/arm64 | 55.2 MiB | 80.4 MiB | 15.6 MiB |

The image adds the distroless base, whose layers are 0.7 MB compressed
(`gcr.io/distroless/static-debian12:nonroot`, as pinned in
`Dockerfile.release`), and about 0.3 MB of license notices, to the binary.
The size of the published v0.2.0 image will be measured from the registry
once it exists.
