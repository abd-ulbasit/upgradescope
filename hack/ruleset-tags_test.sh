#!/usr/bin/env bash
# The tag ruleset (.github/rulesets/release-tags.json) against the tags the
# release workflow makes (make hack-test, #244): every release tag
# (vX.Y.Z[-pre], what release.yml's preflight accepts) is protected, and no
# floating major tag (v0, v1, v12: what major-tag derives from a release as
# ${tag%%.*}) is, or the major-tag job could create v1 and never move it.
# GitHub matches ref_name patterns with fnmatch, where * matches anything
# but a /; a bash pattern match does the same for refs without a /. Offline;
# needs jq.
set -euo pipefail
cd "$(dirname "$0")/.."

rs=${RULESET:-.github/rulesets/release-tags.json}
wf=.github/workflows/release.yml
: "${TMPDIR:=/tmp}"
results=$(mktemp)
trap 'rm -f "$results"' EXIT
ok() { echo "ok   $1" | tee -a "$results"; }
fail() { echo "FAIL $1" >&2; echo "FAIL $1" >>"$results"; }

include=$(jq -r '.conditions.ref_name.include[]' "$rs")
exclude=$(jq -r '.conditions.ref_name.exclude[]' "$rs")
[ -n "$include" ] || { echo "FAIL $rs includes nothing" >&2; exit 1; }

# matches <ref> <patterns>: whether one of the fnmatch patterns matches.
matches() {
  local ref=$1 pat
  while IFS= read -r pat; do
    [ -n "$pat" ] || continue
    # shellcheck disable=SC2053 # the pattern is a glob on purpose
    if [[ $ref == $pat ]]; then
      # * stops at a /: the part a * took must hold none.
      local re=${pat//./\\.}
      re=${re//\*/[^/]*}
      [[ $ref =~ ^$re$ ]] && return 0
    fi
  done <<<"$2"
  return 1
}
protected() { matches "$1" "$include" && ! matches "$1" "$exclude"; }

# The tag check preflight runs (release.yml), so the samples are tags a
# release really runs on.
tag_re=$(sed -n "s/.*grep -Eq '\([^']*\)'.*/\1/p" "$wf" | head -n 1)
[ -n "$tag_re" ] || { echo "FAIL cannot find preflight's tag regex in $wf" >&2; exit 1; }

for tag in v0.2.0 v0.2.0-rc.1 v0.2.1 v1.0.0 v1.0.1 v1.2.3-rc.10 v12.3.4 v12.3.4-beta.1 v99.0.0; do
  printf '%s' "$tag" | grep -Eq "$tag_re" || { fail "$tag is not a tag preflight accepts ($tag_re)"; continue; }
  if protected "refs/tags/$tag"; then ok "release tag $tag is protected"; else fail "release tag $tag is not protected by $rs"; fi
  major=${tag%%.*}
  if protected "refs/tags/$major"; then
    fail "major tag $major (major-tag's \${tag%%.*} for $tag) is protected by $rs, so major-tag cannot move it"
  else
    ok "major tag $major of $tag stays movable"
  fi
done
for major in v0 v1 v2 v9 v10 v99; do
  if protected "refs/tags/$major"; then fail "major tag $major is protected"; else ok "major tag $major stays movable"; fi
done
# major-tag still derives the major as ${GITHUB_REF_NAME%%.*}, which the
# cases above assume.
if grep -qF 'major="${GITHUB_REF_NAME%%.*}"' "$wf"; then
  ok "major-tag derives the major tag as \${GITHUB_REF_NAME%%.*}"
else
  fail "major-tag no longer derives the major tag as \${GITHUB_REF_NAME%%.*}; update this test"
fi
# The rules themselves: no delete, no move, no force-move, nobody exempt.
if [ "$(jq -c '[.rules[].type] | sort' "$rs")" = '["deletion","non_fast_forward","update"]' ] &&
  [ "$(jq -c '.bypass_actors' "$rs")" = '[]' ] && [ "$(jq -r .enforcement "$rs")" = active ]; then
  ok "release tags cannot be deleted, moved or force-moved, by anyone"
else
  fail "$rs no longer forbids deletion, update and non_fast_forward for everyone"
fi

pass=$(grep -c '^ok' "$results" || true)
nfail=$(grep -c '^FAIL' "$results" || true)
echo "ruleset-tags_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
