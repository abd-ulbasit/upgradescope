#!/usr/bin/env bash
# govulncheck against what users actually download (make vuln-latest;
# .github/workflows/vuln-latest-release.yml, daily). CI's vuln job scans a
# fresh build of main; the binary in the latest release can still carry
# advisories fixed since (v0.1.1 had 22 reachable ones, #127). This scans
# that binary:
#
#   1. the latest release's tag (GitHub's "latest": newest non-draft,
#      non-prerelease), or $UPGRADESCOPE_RELEASE_TAG;
#   2. its checksums.txt and linux/amd64 archive (the image's binary),
#      sha256-checked; with cosign installed, checksums.txt must also verify
#      against the release workflow's keyless signature. Only a release
#      older than the first signed one (v0.2.0) may lack the signature
#      bundle, and only as a 404: any other download failure is exit 2;
#   3. hack/vulncheck.sh on the extracted binary: binary mode, the same
#      allowlist and fail-closed rules as the CI gate.
#
# Exit status: hack/vulncheck.sh's (0 clean, 1 findings, 2 broken); 2 when
# the release cannot be downloaded or verified. Writes the tag to
# $UPGRADESCOPE_RELEASE_TAG_FILE when set (for the issue the workflow files).
# Needs gh, curl, jq and Go.
set -euo pipefail
cd "$(dirname "$0")/.."

repo=${UPGRADESCOPE_REPO:-abd-ulbasit/upgradescope}
die() { echo "::error::vuln-latest-release: $*" >&2; exit 2; }
command -v gh >/dev/null || die "gh is required"

tag=${UPGRADESCOPE_RELEASE_TAG:-$(gh api "repos/$repo/releases/latest" --jq .tag_name)}
[ -n "$tag" ] || die "no latest release in $repo"
[ -z "${UPGRADESCOPE_RELEASE_TAG_FILE:-}" ] || printf '%s\n' "$tag" >"$UPGRADESCOPE_RELEASE_TAG_FILE"
echo "vuln-latest-release: $repo $tag (linux/amd64)"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
base="https://github.com/$repo/releases/download/$tag"
asset=upgradescope_linux_amd64.tar.gz
curl -fsSL --retry 3 -o "$work/checksums.txt" "$base/checksums.txt" || die "$tag has no checksums.txt"
curl -fsSL --retry 3 -o "$work/$asset" "$base/$asset" || die "$tag has no $asset"
want="$(awk -v n="$asset" '$2 == n { print $1 }' "$work/checksums.txt")"
if command -v sha256sum >/dev/null; then got="$(sha256sum "$work/$asset" | cut -c1-64)"; else got="$(shasum -a 256 "$work/$asset" | cut -c1-64)"; fi
[ -n "$want" ] && [ "$got" = "$want" ] || die "$asset sha256 $got does not match checksums.txt ($want)"

# Releases from $first_signed on are signed: a missing bundle there, or one
# that cannot be downloaded for any release, fails the scan instead of
# quietly scanning an unverified binary. Only a 404 on an older release
# means "unsigned".
first_signed=v0.2.0
if ! command -v cosign >/dev/null; then
  echo "vuln-latest-release: checksums.txt not signature-checked (cosign is not installed)"
else
  status=$(curl -sSL --retry 3 -o "$work/checksums.txt.sigstore.json" -w '%{http_code}' "$base/checksums.txt.sigstore.json" 2>/dev/null) || status=000
  if [ "$status" = 404 ]; then
    [ "$(printf '%s\n%s\n' "$first_signed" "${tag%%-*}" | sort -V | head -1)" != "$first_signed" ] \
      || die "$tag has no checksums.txt.sigstore.json; every release since $first_signed is signed"
    echo "vuln-latest-release: checksums.txt not signature-checked ($tag predates signed releases)"
  elif [ "$status" != 200 ]; then
    die "cannot download checksums.txt.sigstore.json of $tag (HTTP $status)"
  else
    cosign verify-blob "$work/checksums.txt" --bundle "$work/checksums.txt.sigstore.json" \
      --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$tag" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com >/dev/null \
      || die "checksums.txt of $tag does not verify against the release workflow's signature"
    echo "vuln-latest-release: checksums.txt signature verified"
  fi
fi

tar -xzf "$work/$asset" -C "$work" upgradescope || die "no upgradescope binary in $asset"
UPGRADESCOPE_VULN_BINS="$work/upgradescope" hack/vulncheck.sh
