#!/usr/bin/env bash
# Installs a pinned, checksum-verified CLI the CI scripts need and prints its
# absolute path, so it runs from any directory:
#   kind=$(hack/install-tool.sh kind)
#
# Every tool is pinned to one release, and every platform to the sha256 its
# upstream publishes for that release (kind: the .sha256sum assets;
# kubectl: dl.k8s.io/.../kubectl.sha256; kubeconform: the release's
# CHECKSUMS file; oras: the release's oras_<v>_checksums.txt; helm-docs: the
# release's checksums.txt; kyverno: the release's checksums.txt; gator: the
# release's sha256sums.txt), written down here so a tampered or substituted download
# fails instead of running — in CI, kind and kubectl run with root-equivalent
# access to the runner's Docker. Bump version and every checksum together,
# from the upstream release page, never from a download of your own.
#
# Installs into $TOOLS_BIN (default bin/tools, gitignored; a relative path is
# relative to the repository root) and reuses an install whose recorded pin
# still matches. Messages go to stderr.
#
# Exit status: 0 installed, 1 download or checksum failed, 2 bad request.
#
# Knobs, for hack/install-tool_test.sh:
#   UPGRADESCOPE_TOOL_PLATFORM  os/arch to install for (default: this machine)
#   UPGRADESCOPE_TOOL_URL       fetch from here instead (the pinned sha256 still applies)
set -euo pipefail
cd "$(dirname "$0")/.."

TOOLS_BIN=${TOOLS_BIN:-bin/tools}
case "$TOOLS_BIN" in /*) ;; *) TOOLS_BIN="$PWD/$TOOLS_BIN" ;; esac

KIND_VERSION=v0.33.0
KUBECTL_VERSION=v1.37.1
KUBECONFORM_VERSION=v0.8.0
ORAS_VERSION=v1.3.4 # release.yml pushes the Artifact Hub metadata with it
HELM_DOCS_VERSION=v1.14.2 # make helm-docs: the chart values tables
KYVERNO_VERSION=v1.19.1 # make examples-test: the Kyverno CLI that tests the example policy
GATOR_VERSION=v3.23.1 # make examples-test: Gatekeeper's offline policy verifier

# sha256 <tool> <os/arch>
sha256_for() {
  case "$1 $2" in
    "kind linux/amd64") echo aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d ;;
    "kind linux/arm64") echo 20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2 ;;
    "kind darwin/amd64") echo 5a99f26f57246dc9319dd294803313197a0f34d33c525b3ea8b655db5916ece0 ;;
    "kind darwin/arm64") echo 0c8c7dbe5e23594a198b786c4bc13dacc101fa6196b0cb0b23a1ca44e61f4b4f ;;
    "kubectl linux/amd64") echo 65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8 ;;
    "kubectl linux/arm64") echo ff749f4b78d9c4f1ec87307df9b50119ed819e2094aa9810cb9acffc3286c8c7 ;;
    "kubectl darwin/amd64") echo 6851381c486ff6edd691623e3d65c87cb9a5b02887ff8fbbb38d8a031b748387 ;;
    "kubectl darwin/arm64") echo fd65982c97ddad3106754b69ffa196d0e543aa591930ae52aed1adfb92f8c77f ;;
    "kubeconform linux/amd64") echo 9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883 ;;
    "kubeconform linux/arm64") echo 1f53fc8e81258197a35e8603054162a5af1de8c5af13746c71ab680d9534ed87 ;;
    "kubeconform darwin/amd64") echo 71dbc87ac9f24099a62b93570e65aa06312ba6ac8aea63b7f86e9d999edf5a92 ;;
    "kubeconform darwin/arm64") echo f84f4dfbebf4a6b0b230385fa065a39ea35e02608c2b50d025dcf64775a69d67 ;;
    "oras linux/amd64") echo f27adb935022d94df8dc77719c322dda592c78a0d57a6f7dcdd8d900b248c454 ;;
    "oras linux/arm64") echo 15702c6e3a4a56a8bd8ac5c17efdbcab56d9bada661ccbcf017f5b10c1d89399 ;;
    "oras darwin/amd64") echo 5e964f3d5a36eb9499a9d3e252a86b09e7adf3e6f6447eec56fd249c6702af7e ;;
    "oras darwin/arm64") echo 217761a9500242ff473de8656b5aca21136ff39e17e9e61fd8936bbfd902704c ;;
    "helm-docs linux/amd64") echo a8cf72ada34fad93285ba2a452b38bdc5bd52cc9a571236244ec31022928d6cc ;;
    "helm-docs linux/arm64") echo c3787212332386dcd122debef7848feb165aa701467ae3e3442df7638f3ac4e4 ;;
    "helm-docs darwin/amd64") echo b2f1ffd0feef8dc0901a38a2053481d1d67b63ca30da4ac774166c6b52fa2245 ;;
    "helm-docs darwin/arm64") echo 2d8399db5b33d240d5f8985241bcf5483563150b968e3229823822979f3e4b8b ;;
    "kyverno linux/amd64") echo b38228f367fc0fdc2b08f4c83ea50ac5f16c60ff8d62d76a66157c33c47b70ae ;;
    "kyverno linux/arm64") echo d78fecd183e1e6749c653fdb744c88d065e6737ec6856c97c4efac84bec54554 ;;
    "kyverno darwin/amd64") echo d9d7a857755ccd027cba8b2a5146a515c1590d65b38cb37a2170809329e4f393 ;;
    "kyverno darwin/arm64") echo 2cf5febbedaafca5d3c7819925a49f9723bf4c3337a1327b6f72970480af385a ;;
    "gator linux/amd64") echo c268d7b809c9fe59a110ab0bf7c296ab00b0b6c894c209cd15e94194590291bb ;;
    "gator linux/arm64") echo 2d4bd0e28e708c2893a66fdca652f1d775b23fc5a3c07571fd314ba06d19ff2b ;;
    "gator darwin/amd64") echo 4318b3ce4bc115ed861ff4a1df26ad1ddf3ae0c7907f20caac85affd400af39e ;;
    "gator darwin/arm64") echo dc96df4a0bad0427b36d2b10473801528ca5bdb00f2addb34d17bb7d706ed99b ;;
  esac
}

bad() { echo "install-tool: $*" >&2; exit 2; }
die() { echo "::error::install-tool: $*" >&2; exit 1; }

tool=${1:-}
case "$tool" in
  kind) version=$KIND_VERSION ;;
  kubectl) version=$KUBECTL_VERSION ;;
  kubeconform) version=$KUBECONFORM_VERSION ;;
  oras) version=$ORAS_VERSION ;;
  helm-docs) version=$HELM_DOCS_VERSION ;;
  kyverno) version=$KYVERNO_VERSION ;;
  gator) version=$GATOR_VERSION ;;
  *) bad "unknown tool '$tool' (kind, kubectl, kubeconform, oras, helm-docs, kyverno, gator)" ;;
esac

platform=${UPGRADESCOPE_TOOL_PLATFORM:-$(uname -s | tr '[:upper:]' '[:lower:]')/$(uname -m)}
platform=${platform/x86_64/amd64}
platform=${platform/aarch64/arm64}
os=${platform%/*}
arch=${platform#*/}
want=$(sha256_for "$tool" "$platform")
[ -n "$want" ] || bad "no pinned $tool for $platform"

case "$tool" in
  kind) url="https://kind.sigs.k8s.io/dl/$version/kind-$os-$arch" ;;
  kubectl) url="https://dl.k8s.io/release/$version/bin/$os/$arch/kubectl" ;;
  kubeconform) url="https://github.com/yannh/kubeconform/releases/download/$version/kubeconform-$os-$arch.tar.gz" ;;
  oras) url="https://github.com/oras-project/oras/releases/download/$version/oras_${version#v}_${os}_$arch.tar.gz" ;;
  helm-docs)
    # Asset names spell the platform Darwin_x86_64, Linux_arm64.
    hd_os=$(printf '%s' "${os:0:1}" | tr '[:lower:]' '[:upper:]')${os:1}
    hd_arch=${arch/amd64/x86_64}
    url="https://github.com/norwoodj/helm-docs/releases/download/$version/helm-docs_${version#v}_${hd_os}_$hd_arch.tar.gz"
    ;;
  kyverno)
    # Asset names spell the architecture x86_64.
    url="https://github.com/kyverno/kyverno/releases/download/$version/kyverno-cli_${version}_${os}_${arch/amd64/x86_64}.tar.gz"
    ;;
  gator) url="https://github.com/open-policy-agent/gatekeeper/releases/download/$version/gator-$version-$os-$arch.tar.gz" ;;
esac
url=${UPGRADESCOPE_TOOL_URL:-$url}

dest="$TOOLS_BIN/$tool"
pin="$version $want"
if [ -x "$dest" ] && [ "$(cat "$TOOLS_BIN/.$tool.pin" 2>/dev/null)" = "$pin" ]; then
  echo "$dest"
  exit 0
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
echo "install-tool: $tool $version ($platform) from $url" >&2
curl -fsSL --retry 3 -o "$work/download" "$url" || die "could not download $tool from $url"

if command -v sha256sum >/dev/null; then
  got=$(sha256sum "$work/download" | awk '{print $1}')
else
  got=$(shasum -a 256 "$work/download" | awk '{print $1}')
fi
[ "$got" = "$want" ] || die "sha256 mismatch for $tool $version ($platform): got $got, want $want"

case "$tool" in
  kubeconform)
    tar -xzf "$work/download" -C "$work" kubeconform || die "no kubeconform in the $version archive"
    mv "$work/kubeconform" "$work/bin"
    ;;
  oras)
    tar -xzf "$work/download" -C "$work" oras || die "no oras in the $version archive"
    mv "$work/oras" "$work/bin"
    ;;
  helm-docs)
    tar -xzf "$work/download" -C "$work" helm-docs || die "no helm-docs in the $version archive"
    mv "$work/helm-docs" "$work/bin"
    ;;
  kyverno)
    tar -xzf "$work/download" -C "$work" kyverno || die "no kyverno in the $version archive"
    mv "$work/kyverno" "$work/bin"
    ;;
  gator)
    tar -xzf "$work/download" -C "$work" gator || die "no gator in the $version archive"
    mv "$work/gator" "$work/bin"
    ;;
  *) mv "$work/download" "$work/bin" ;;
esac
chmod +x "$work/bin"
mkdir -p "$TOOLS_BIN"
mv "$work/bin" "$dest"
echo "$pin" >"$TOOLS_BIN/.$tool.pin"
echo "$dest"
