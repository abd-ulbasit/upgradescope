#!/usr/bin/env bash
# Scans what a release ships beyond the Go code govulncheck can reach
# (make image-scan; release.yml's verify job on every release, and the
# image job of .github/workflows/vuln-latest-release.yml daily), with grype
# (pinned and sha256-verified by hack/install-tool.sh):
#   - the published image, its linux/amd64 and linux/arm64 images, pulled
#     from the registry (no Docker): the distroless base's OS packages and
#     the binary's modules;
#   - the SPDX SBOM the release publishes for each linux and darwin archive
#     (<archive>.sbom.json), after checking checksums.txt's keyless signature
#     (the release workflow at that tag) and each SBOM's sha256 against it:
#     the SBOMs are consumed, not only produced.
#
# It fails on a finding that has a fix and is high or critical. Accepting
# one is an entry in hack/grype.yaml (grype's own ignore rules) with a
# reason and a review date. govulncheck (hack/vulncheck.sh) stays the
# pre-release gate for Go advisories: it knows what the binary can reach,
# while grype reports every module the SBOM lists, reachable or not, and
# sees the OS packages govulncheck cannot.
#
# Usage: hack/image-scan.sh [vX.Y.Z]   (default: the latest release)
# Knobs: UPGRADESCOPE_REPO (default abd-ulbasit/upgradescope),
#        IMAGE_SCAN_IMAGE (default ghcr.io/<repo>),
#        IMAGE_SCAN_DIGEST (scan the image index by this digest, not the
#        tag: release.yml passes the one it verified),
#        IMAGE_SCAN_GRYPE (a grype binary; default hack/install-tool.sh grype).
# Needs gh (GH_TOKEN), jq, cosign and sha256sum or shasum.
# Exit: 0 nothing fixable at high or above, 1 findings, 2 could not scan.
set -euo pipefail
cd "$(dirname "$0")/.."

repo=${UPGRADESCOPE_REPO:-abd-ulbasit/upgradescope}
image=${IMAGE_SCAN_IMAGE:-ghcr.io/$repo}
config=$PWD/hack/grype.yaml
die() { echo "::error::image-scan: $*" >&2; exit 2; }
command -v jq >/dev/null || die "jq is not installed"
command -v cosign >/dev/null || die "cosign is not installed (checksums.txt's signature is checked before its SBOMs are read)"

tag=${1:-$(gh api "repos/$repo/releases/latest" --jq .tag_name)} || die "cannot read the latest release of $repo"
printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || die "'$tag' is not a release tag"
grype=${IMAGE_SCAN_GRYPE:-$(hack/install-tool.sh grype)} || die "cannot install grype"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The release's SBOMs, held to its signed checksums.txt.
gh release download "$tag" -R "$repo" -D "$work" -p checksums.txt -p checksums.txt.sigstore.json -p '*.sbom.json' \
  || die "cannot download checksums.txt and the SBOMs of $tag"
cosign verify-blob "$work/checksums.txt" --bundle "$work/checksums.txt.sigstore.json" \
  --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$tag" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com >/dev/null 2>"$work/cosign.err" \
  || die "checksums.txt of $tag does not verify against $repo's release workflow: $(tr '\n' ' ' <"$work/cosign.err")"
sboms=()
for f in "$work"/upgradescope_linux_*.sbom.json "$work"/upgradescope_darwin_*.sbom.json; do
  [ -f "$f" ] || continue
  name=${f##*/}
  want=$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1 }' "$work/checksums.txt")
  if command -v sha256sum >/dev/null; then got=$(sha256sum "$f" | awk '{print $1}'); else got=$(shasum -a 256 "$f" | awk '{print $1}'); fi
  [ -n "$want" ] && [ "$got" = "$want" ] || die "$name does not match checksums.txt of $tag"
  sboms+=("$f")
done
[ ${#sboms[@]} -gt 0 ] || die "$tag publishes no linux or darwin SBOM"

ref="$image:${tag#v}"
[ -z "${IMAGE_SCAN_DIGEST:-}" ] || ref="$image@$IMAGE_SCAN_DIGEST"

# scan <label> <grype source> [grype flags]: one JSON report, or exit 2. A
# failed run is tried once more: a registry pull can drop mid-layer.
n=0
scan() {
  local label=$1 src=$2 out attempt
  shift 2
  n=$((n + 1))
  out="$work/report-$n.json"
  for attempt in 1 2; do
    if "$grype" "$src" "$@" -c "$config" --only-fixed -o json --file "$out" >"$work/grype-$n.log" 2>&1; then break; fi
    if [ "$attempt" = 2 ]; then
      sed 's/^/  /' "$work/grype-$n.log" >&2
      die "grype could not scan $label"
    fi
  done
  jq -r --arg l "$label" '.matches[]? | select(.vulnerability.severity == "High" or .vulnerability.severity == "Critical")
    | [$l, .vulnerability.severity, .vulnerability.id, .artifact.name, .artifact.version,
       ((.vulnerability.fix.versions // []) | join(","))] | @tsv' "$out" >>"$work/findings.tsv"
  echo "image-scan: scanned $label"
}
: >"$work/findings.tsv"
for platform in linux/amd64 linux/arm64; do
  scan "$ref ($platform)" "registry:$ref" --platform "$platform"
done
for f in "${sboms[@]}"; do
  scan "${f##*/}" "sbom:$f"
done

if [ -s "$work/findings.tsv" ]; then
  sort -u "$work/findings.tsv" | while IFS=$'\t' read -r where sev id pkg ver fix; do
    echo "::error::$tag: $sev $id in $pkg $ver (fixed in $fix), $where"
  done
  echo "image-scan: $tag has fixable high or critical findings; release a patch with the fix, or accept one in hack/grype.yaml with a reason and a review date" >&2
  exit 1
fi
echo "image-scan: $tag: no fixable high or critical finding in $ref (linux/amd64, linux/arm64) or its ${#sboms[@]} SBOMs"
