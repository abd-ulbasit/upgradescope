#!/usr/bin/env bash
# The binary and archive sizes the docs state are the release build's
# (#130 PF-03): hack/release-check.sh runs this after the GoReleaser
# snapshot, so a release whose sizes drifted more than 2% from the docs
# fails its CI until the numbers are re-measured. It reads
#
#   docs/operations/install.md, Sizes: table rows
#       | <os>/<arch> | <binary> MiB | <archive> MiB |
#   README.md, Measured: the row
#       | Binary and archive, <os>/<arch> ... | <binary> MiB binary, <archive> MiB archive | ... |
#
# where "binary" is the stripped binary GoReleaser builds and "archive" the
# release archive (.tar.gz; .zip on Windows), and compares each with the
# file dist/artifacts.json lists for that platform. A page that documents no
# size at all fails too, so a reformatted table cannot turn the check off.
#
# Usage: hack/check-doc-sizes.sh          check against $DIST
#        hack/check-doc-sizes.sh --list   print "<page> <os/arch> <kind> <MiB>"
#                                         for every size read, check nothing
# Knobs (hack/check-doc-sizes_test.sh drives them): DIST (default dist),
# DOC_SIZES_README (README.md), DOC_SIZES_INSTALL
# (docs/operations/install.md), DOC_SIZES_TOLERANCE_PCT (2). Needs jq.
set -euo pipefail
cd "$(dirname "$0")/.."

DIST=${DIST:-dist}
readme=${DOC_SIZES_README:-README.md}
install=${DOC_SIZES_INSTALL:-docs/operations/install.md}
tolerance=${DOC_SIZES_TOLERANCE_PCT:-2}
die() { echo "::error::check-doc-sizes: $*" >&2; exit 1; }

# sizes <page>: "<page> <os/arch> <kind> <MiB>" for each size it documents.
sizes() {
  local page=$1 plat='(linux|darwin|windows)/(amd64|arm64)' num='([0-9]+(\.[0-9]+)?)'
  [ -f "$page" ] || die "no $page"
  if [ "$page" = "$readme" ]; then
    sed -nE "s#^\|[^|]*[^a-z]($plat)[^|]*\| $num MiB binary, $num MiB archive \|.*#\1 \4 \6#p" "$page"
  else
    sed -nE "s#^\| ($plat) \| $num MiB \| $num MiB \|\$#\1 \4 \6#p" "$page"
  fi | awk -v page="$page" '{ print page, $1, "binary", $2; print page, $1, "archive", $3 }'
}

docs=$( (sizes "$readme" && sizes "$install") | sort -u)
for page in "$readme" "$install"; do
  awk -v p="$page" '$1 == p { found = 1 } END { exit !found }' <<<"$docs" ||
    die "$page documents no size in the format this check reads (see hack/check-doc-sizes.sh)"
done
if [ "${1:-}" = --list ]; then
  echo "$docs"
  exit 0
fi

artifacts=$DIST/artifacts.json
[ -f "$artifacts" ] || die "no $artifacts: run the GoReleaser snapshot first (make release-check)"

bad=0
while read -r page plat kind doc; do
  type=Binary
  [ "$kind" = binary ] || type=Archive
  path=$(jq -r --arg os "${plat%/*}" --arg arch "${plat#*/}" --arg type "$type" \
    'first(.[] | select(.type == $type and .goos == $os and .goarch == $arch) | .path) // empty' "$artifacts")
  [ -n "$path" ] && [ -f "$path" ] || {
    echo "::error file=$page::no $plat $kind in $artifacts, but $page documents one" >&2
    bad=1
    continue
  }
  bytes=$(wc -c <"$path" | tr -d ' ')
  if verdict=$(awk -v doc="$doc" -v bytes="$bytes" -v tol="$tolerance" 'BEGIN {
      built = bytes / 1048576; off = (doc - built) / built * 100; if (off < 0) off = -off
      printf "%.2f MiB, %.1f%% off", built, off
      exit off > tol }'); then
    echo "ok: $page $plat $kind $doc MiB (built ${verdict})"
  else
    echo "::error file=$page::$page: $plat $kind: documented $doc MiB, built ${verdict}, more than ${tolerance}%; re-measure it from the release build (hack/check-doc-sizes.sh)" >&2
    bad=1
  fi
done <<<"$docs"
[ "$bad" = 0 ] || exit 1
echo "check-doc-sizes: every documented size is within ${tolerance}% of $DIST"
