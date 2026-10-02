#!/usr/bin/env bash
# Stamps a copy of the chart with what is only known at release time
# (release.yml's chart job runs it before `helm package`):
#
#   values.yaml  image.digest = the digest of the image this release pushed,
#                so the published chart pulls exactly those bytes even if the
#                tag were ever moved (#127);
#   Chart.yaml   artifacthub.io/images names that image as tag@digest,
#                artifacthub.io/prerelease is "true" for a vX.Y.Z-pre tag,
#                and artifacthub.io/changes lists the release's CHANGELOG.md
#                entries (the section hack/check-changelog.sh resolves; one per
#                bullet under ### Added/Changed/Deprecated/Removed/Fixed/Security)
#                for Artifact Hub's changelog view (a bullet under any other
#                heading is dropped with a warning), and
#                artifacthub.io/containsSecurityUpdates is "true" when one of
#                them is a ### Security entry.
#
# Usage: hack/chart-release-annotations.sh <chart-dir> <tag> <sha256:digest> [changelog]
# Edits <chart-dir> in place: pass a copy, never deploy/chart itself.
# Offline; hack/chart-release-annotations_test.sh drives it.
set -euo pipefail
export LC_ALL=C

die() { echo "::error::chart-release-annotations: $*" >&2; exit 1; }
[ $# -ge 3 ] || die "usage: $0 <chart-dir> <tag> <sha256:digest> [changelog]"
chart=$1 tag=$2 digest=$3 changelog=${4:-CHANGELOG.md}
command -v jq >/dev/null || die "jq is required"

printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || die "'$tag' is not a vX.Y.Z[-pre] tag"
printf '%s' "$digest" | grep -Eq '^sha256:[a-f0-9]{64}$' || die "'$digest' is not a sha256:<64 hex> digest"
values="$chart/values.yaml"
chartyaml="$chart/Chart.yaml"
[ -f "$values" ] && [ -f "$chartyaml" ] && [ -f "$changelog" ] || die "need $values, $chartyaml and $changelog"

# edit <file> <sed expr> <description>: exactly one line must change.
edit() {
  local file=$1 expr=$2 what=$3
  sed "$expr" "$file" >"$file.new"
  local n
  n=$(diff "$file" "$file.new" | grep -c '^>' || true)
  [ "$n" = 1 ] || { rm -f "$file.new"; die "$what: expected to change exactly one line of $file, changed $n"; }
  mv "$file.new" "$file"
}

edit "$values" "s|^  digest: \"\"\$|  digest: \"$digest\"|" "image.digest"

image="$(sed -n 's/^ *repository: *//p' "$values" | head -1 | tr -d '"')"
[ -n "$image" ] || die "no image.repository in $values"
edit "$chartyaml" "s|^      image: $image:.*\$|      image: $image:$tag@$digest|" "artifacthub.io/images"
pre=false
case "$tag" in *-*) pre=true ;; esac
sed "s|^  artifacthub.io/prerelease: .*\$|  artifacthub.io/prerelease: \"$pre\"|" "$chartyaml" >"$chartyaml.new" && mv "$chartyaml.new" "$chartyaml"
[ "$(grep -cxF "  artifacthub.io/prerelease: \"$pre\"" "$chartyaml")" = 1 ] || die "no single artifacthub.io/prerelease annotation in $chartyaml"

# The release's section, as release.yml's preflight resolved it.
"$(dirname "$0")/check-changelog.sh" --print "$tag" "$changelog" >"$chart/.changes.md" \
  || { rm -f "$chart/.changes.md"; die "no CHANGELOG entries for $tag (hack/check-changelog.sh)"; }

# One change per top-level bullet; indented lines continue it.
awk '
  function flush() {
    if (desc != "" && kind != "") printf "%s\t%s\n", kind, desc
    else if (desc != "") printf "::warning::chart-release-annotations: dropped from artifacthub.io/changes (not under a Keep a Changelog heading): %s\n", desc >"/dev/stderr"
    desc = ""
  }
  /^### / {
    flush()
    k = tolower(substr($0, 5)); gsub(/[^a-z]/, "", k)
    kind = (k ~ /^(added|changed|deprecated|removed|fixed|security)$/) ? k : ""
    next
  }
  /^- / { flush(); desc = substr($0, 3); next }
  /^[[:space:]]+[^[:space:]]/ && desc != "" { line = $0; sub(/^[[:space:]]+/, "", line); desc = desc " " line; next }
  /^[[:space:]]*$/ { flush(); next }
  END { flush() }
' "$chart/.changes.md" >"$chart/.changes.tsv"
rm -f "$chart/.changes.md"
[ -s "$chart/.changes.tsv" ] || { rm -f "$chart/.changes.tsv"; die "no ### Added/Changed/Fixed/... bullets for $tag in $changelog"; }

# Artifact Hub flags a release that fixes vulnerabilities: a ### Security
# entry says this one does.
if grep -q '^security	' "$chart/.changes.tsv"; then
  edit "$chartyaml" 's|^  artifacthub.io/containsSecurityUpdates: "false"$|  artifacthub.io/containsSecurityUpdates: "true"|' \
    "artifacthub.io/containsSecurityUpdates"
fi

# annotations is the last block of Chart.yaml (the file says so); append.
grep -q '^annotations:' "$chartyaml" || die "$chartyaml has no annotations block"
[ -z "$(awk '/^annotations:/ { a = 1; next } a && /^[^ #]/ { print }' "$chartyaml")" ] \
  || die "annotations is not the last block of $chartyaml"
{
  echo "  artifacthub.io/changes: |"
  while IFS=$'\t' read -r kind desc; do
    # Markdown links and code spans read fine as plain text; JSON-quote the
    # description so any character is valid YAML.
    printf '    - kind: %s\n      description: %s\n' "$kind" "$(jq -Rn --arg d "$desc" '$d')"
  done <"$chart/.changes.tsv"
} >>"$chartyaml"
n=$(grep -c . "$chart/.changes.tsv")
rm -f "$chart/.changes.tsv"
echo "chart-release-annotations: $chart -> $image:$tag@$digest (prerelease $pre, $n changes)"
