#!/usr/bin/env bash
# The commit a release tag names must be on main (release.yml's preflight;
# #198, IR-11). Tag creation is not restricted, and the tag ruleset only makes
# a tag immutable once it exists, so without this a v* tag pushed on a
# feature-branch commit would run the whole release: the CI gate passes
# there, but the release would ship code that main never reviewed.
#
# Usage: hack/check-release-ancestry.sh [ref]   (default HEAD, the checked-out tag)
# Needs main's history: actions/checkout with fetch-depth: 0. With less, the
# check fails rather than passing on a ref it cannot see.
# UPGRADESCOPE_MAIN_REF overrides origin/main. Offline;
# hack/check-release-ancestry_test.sh drives it.
set -euo pipefail

ref=${1:-HEAD}
main=${UPGRADESCOPE_MAIN_REF:-origin/main}
die() { echo "::error::release-ancestry: $*" >&2; exit 1; }

sha=$(git rev-parse --verify --quiet "$ref^{commit}") || die "'$ref' is not a commit"
git rev-parse --verify --quiet "$main^{commit}" >/dev/null \
  || die "no $main to compare against; fetch it (actions/checkout with fetch-depth: 0)"
if [ "$(git rev-parse --is-shallow-repository)" = true ]; then
  die "this clone is shallow, so $main's history is incomplete; fetch it all (actions/checkout with fetch-depth: 0)"
fi
git merge-base --is-ancestor "$sha" "$main" \
  || die "$ref (${sha:0:7}) is not on $main. Releases are cut from main only: merge the change, update the release commit there, and tag that commit."
echo "ok: $ref (${sha:0:7}) is on $main"
