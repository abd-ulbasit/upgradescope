#!/usr/bin/env bash
# Brings Formula/upgradescope.rb up to the latest stable upgradescope release
# (.github/workflows/update.yml runs it every 6 hours and on demand):
#
#   1. read the latest release (GitHub's "latest" is the newest non-draft,
#      non-prerelease one);
#   2. download its checksums.txt and checksums.txt.sigstore.json;
#   3. verify the bundle keylessly: the signing certificate must have been
#      issued to upgradescope's release workflow for that very tag, through
#      GitHub Actions OIDC. Nothing unverified reaches the formula;
#   4. re-render the formula from those checksums.
#
# The caller commits the formula when it changed. Needs gh (GH_TOKEN),
# curl and cosign.
#
# Knobs, for tests:
#   UPGRADESCOPE_REPO   source repository (default abd-ulbasit/upgradescope)
#   UPGRADESCOPE_TAG    release tag to render instead of the latest
set -euo pipefail
cd "$(dirname "$0")/.."

repo=${UPGRADESCOPE_REPO:-abd-ulbasit/upgradescope}
die() { echo "::error::update: $*" >&2; exit 1; }

tag=${UPGRADESCOPE_TAG:-$(gh api "repos/$repo/releases/latest" --jq .tag_name)}
printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || die "latest release tag '$tag' is not vX.Y.Z"
echo "update: $repo $tag" >&2

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
base="https://github.com/$repo/releases/download/$tag"
curl -fsSL --retry 3 -o "$work/checksums.txt" "$base/checksums.txt" || die "$tag has no checksums.txt"
curl -fsSL --retry 3 -o "$work/checksums.txt.sigstore.json" "$base/checksums.txt.sigstore.json" \
  || die "$tag has no checksums.txt.sigstore.json; releases before the CI release pipeline are unsigned and are not published to the tap"

cosign verify-blob "$work/checksums.txt" \
  --bundle "$work/checksums.txt.sigstore.json" \
  --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$tag" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  || die "checksums.txt of $tag does not verify against $repo's release workflow; refusing to update the formula"

script/render-formula.sh "${tag#v}" "$work/checksums.txt"
