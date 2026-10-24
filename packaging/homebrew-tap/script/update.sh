#!/usr/bin/env bash
# Brings Formula/upgradescope.rb up to the latest stable upgradescope release
# (.github/workflows/update.yml runs it every 6 hours and on demand):
#
#   1. read the latest release (GitHub's "latest" is the newest non-draft,
#      non-prerelease one), and refuse it when it is older than the version
#      the formula already ships: GitHub's Latest moved back by hand, or a
#      dispatched older tag, would otherwise downgrade every `brew upgrade`;
#   2. resolve the tag to its commit and refuse a commit that is not on
#      upgradescope's main: release tags can be created on any commit, and the
#      release workflow's own "tagged commit is on main" check runs from the
#      tagged commit, so it cannot be relied on to have run;
#   3. download its checksums.txt and checksums.txt.sigstore.json;
#   4. verify the bundle keylessly: the signing certificate must have been
#      issued to upgradescope's release workflow for that very tag, at that
#      very commit, through GitHub Actions OIDC. Nothing unverified reaches
#      the formula;
#   5. re-render the formula from those checksums.
#
# The caller commits the formula when it changed. Needs gh (GH_TOKEN),
# curl and cosign.
#
# Knobs:
#   UPGRADESCOPE_TAG              release tag to render instead of the latest
#   UPGRADESCOPE_ALLOW_DOWNGRADE  1 = render a tag older than the formula's
#                                 version (a deliberate rollback: the
#                                 workflow's allow-downgrade input)
#   UPGRADESCOPE_REPO             source repository (default
#                                 abd-ulbasit/upgradescope), for tests
set -euo pipefail
cd "$(dirname "$0")/.."

repo=${UPGRADESCOPE_REPO:-abd-ulbasit/upgradescope}
formula=Formula/upgradescope.rb
die() { echo "::error::update: $*" >&2; exit 1; }

# vercmp <X.Y.Z> <X.Y.Z>: -1, 0 or 1, comparing each part as a number.
vercmp() {
  local IFS=. i
  local -a a b
  read -r -a a <<<"$1"
  read -r -a b <<<"$2"
  for i in 0 1 2; do
    if ((10#${a[i]} > 10#${b[i]})); then echo 1; return; fi
    if ((10#${a[i]} < 10#${b[i]})); then echo -1; return; fi
  done
  echo 0
}

tag=${UPGRADESCOPE_TAG:-$(gh api "repos/$repo/releases/latest" --jq .tag_name)}
printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || die "latest release tag '$tag' is not vX.Y.Z"
echo "update: $repo $tag" >&2

# The version the formula ships now (none before the first render).
current=
if [ -f "$formula" ]; then
  current=$(sed -n 's#.*/releases/download/v\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)/.*#\1#p' "$formula" | head -1)
  [ -n "$current" ] || die "$formula names no release version; refusing to overwrite it"
fi
if [ -n "$current" ] && [ "$(vercmp "${tag#v}" "$current")" = -1 ]; then
  if [ "${UPGRADESCOPE_ALLOW_DOWNGRADE:-}" = 1 ]; then
    echo "update: downgrading the formula from v$current to $tag on request (UPGRADESCOPE_ALLOW_DOWNGRADE=1)" >&2
  else
    die "$tag is older than the formula's v$current; refusing to downgrade (has GitHub's Latest been moved back?). To roll back on purpose, run the workflow with allow-downgrade"
  fi
fi

commit=$(gh api "repos/$repo/commits/$tag" --jq .sha) || die "cannot resolve $tag to a commit"
printf '%s' "$commit" | grep -Eq '^[0-9a-f]{40}$' || die "$tag resolved to '$commit', not a commit SHA"
# compare/<commit>...main: "ahead" or "identical" means main contains it.
status=$(gh api "repos/$repo/compare/$commit...main" --jq .status) || die "cannot compare $tag ($commit) with $repo's main"
case "$status" in
  ahead | identical) ;;
  *) die "$tag ($commit) is not on $repo's main (compare: $status); refusing to publish a release built from another branch" ;;
esac

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
  --certificate-github-workflow-sha "$commit" \
  || die "checksums.txt of $tag does not verify against $repo's release workflow at $commit; refusing to update the formula"

script/render-formula.sh "${tag#v}" "$work/checksums.txt"
