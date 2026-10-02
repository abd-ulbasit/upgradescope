#!/usr/bin/env bash
# The release's CHANGELOG.md section must say what changed (release.yml's
# preflight; #127). GoReleaser writes the GitHub release notes from
# conventional-commit subjects, but CHANGELOG.md is the reviewed record: the
# release PR moves [Unreleased] into "## [X.Y.Z] - date", or leaves the
# entries under [Unreleased] for a pre-release. A tag whose section is empty
# ships changes nobody wrote down, so it is refused.
#
# The release's section is the first of these that exists:
#   ## [X.Y.Z-pre]   (the exact tag, pre-releases only)
#   ## [X.Y.Z]
#   ## [Unreleased]
# and it must hold at least one "- " entry.
#
# Usage: hack/check-changelog.sh [--print] <vX.Y.Z[-pre]> [changelog]
#   --print  write the section's body to stdout (hack/chart-release-annotations.sh
#            turns it into Artifact Hub's changes)
# Offline; hack/check-changelog_test.sh drives it.
set -euo pipefail

print=0
[ "${1:-}" = --print ] && { print=1; shift; }
tag=${1:?usage: $0 [--print] <vX.Y.Z[-pre]> [changelog]}
changelog=${2:-CHANGELOG.md}
die() { echo "::error file=$changelog::$*" >&2; exit 1; }
[ -f "$changelog" ] || die "no $changelog"
printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || die "'$tag' is not a vX.Y.Z[-pre] tag"
version=${tag#v}
base=${version%%-*}

# section <name>: the lines between "## [<name>]" and the next "## [".
section() { awk -v h="## [$1]" 'index($0, h) == 1 { on = 1; next } on && /^## \[/ { exit } on { print }' "$changelog"; }
has() { grep -q "^## \[$1\]" "$changelog"; }

candidates=("$base" Unreleased)
[ "$version" != "$base" ] && candidates=("$version" "${candidates[@]}")
for name in "${candidates[@]}"; do
  has "$name" || continue
  body="$(section "$name")"
  grep -q '^- ' <<<"$body" || die "$tag: the ## [$name] section of $changelog has no entries; list what this release changes (Keep a Changelog: ### Added/Changed/Fixed/...) before tagging"
  if [ "$print" = 1 ]; then
    printf '%s\n' "$body"
  else
    echo "ok: $tag is described by ## [$name] in $changelog ($(grep -c '^- ' <<<"$body") entries)"
  fi
  exit 0
done
die "$tag: $changelog has no ## [$version], ## [$base] or ## [Unreleased] section"
