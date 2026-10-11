#!/usr/bin/env bash
# Keeps docs/claims.md from naming a tracker that is no longer open (CI's
# repo-checks job; online, so not part of make claims-check). A claim that is
# not proven says which issue tracks the gap, after one of these markers in
# its "Proven by" cell:
#
#   not true yet: #N   not automated yet: #N   not automated: #N
#   not audited: #N    under repair: #N
#
# (a comma works for the colon, and several numbers may follow one marker).
# Each of those issues must be OPEN on GitHub: a closed one means the gap has
# no tracker any more, so the row is either fixed (move it up with its proof)
# or still true (re-point it at an open issue, #99 for a hand-checked row).
# A #N anywhere else in a row is history and is not asked about.
#
# Needs `gh` and a token (GH_TOKEN or GITHUB_TOKEN) or a `gh auth login`;
# without them it skips with a notice, or fails when CLAIMS_TRACKERS_REQUIRE=1.
#
# Knobs: CLAIMS_FILE (default docs/claims.md), CLAIMS_TRACKERS_GH (the gh
# command), GITHUB_REPOSITORY (default abd-ulbasit/upgradescope).
set -euo pipefail
cd "$(dirname "$0")/.."

file=${CLAIMS_FILE:-docs/claims.md}
gh_cmd=${CLAIMS_TRACKERS_GH:-gh}
repo=${GITHUB_REPOSITORY:-abd-ulbasit/upgradescope}
[ -f "$file" ] || { echo "claims-trackers: no $file" >&2; exit 1; }

# cannot <why>: skip, or fail when the check is required.
cannot() {
  if [ "${CLAIMS_TRACKERS_REQUIRE:-0}" = 1 ]; then
    echo "claims-trackers: $1, and CLAIMS_TRACKERS_REQUIRE=1" >&2
    exit 1
  fi
  echo "claims-trackers: skipping: $1"
  exit 0
}
command -v "$gh_cmd" >/dev/null 2>&1 || cannot "gh is not installed"
if [ -z "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ] && ! "$gh_cmd" auth status >/dev/null 2>&1; then
  cannot "no GH_TOKEN or GITHUB_TOKEN, and gh is not logged in"
fi

marker='(not true yet|not automated yet|not automated|not audited|under repair)[:,]? *\(?#[0-9]+( *(,|and) *#[0-9]+)*'
states=""     # "<number> <STATE>" lines, one per issue asked about
checked=0 bad=0
state_of() { # state_of <number>: sets $state to the issue's state, "?" when it cannot be read
  local n=$1 s
  s=$(grep -m1 "^$n " <<<"$states" || true)
  if [ -z "$s" ]; then
    s="$n $("$gh_cmd" issue view "$n" -R "$repo" --json state -q .state 2>/dev/null || echo '?')"
    states+="$s"$'\n'
  fi
  state=${s#* }
}

while IFS= read -r line; do
  [[ "$line" == '|'* ]] || continue
  id=$(cut -d'|' -f2 <<<"$line" | xargs)
  [ -n "$id" ] && [ "$id" != ID ] || continue
  proof=${line%|}
  proof=${proof##*|}
  while IFS= read -r n; do
    [ -n "$n" ] || continue
    checked=$((checked + 1))
    state_of "$n"
    case "$state" in
      OPEN) ;;
      '?')
        echo "claims-trackers: $file: $id: #$n: could not read its state ($repo)" >&2
        bad=$((bad + 1)) ;;
      *)
        echo "claims-trackers: $file: $id: #$n is $state, so the gap it tracks has no open tracker: re-point the row at an open issue, or move it up with its proof" >&2
        bad=$((bad + 1)) ;;
    esac
  done < <(grep -oE "$marker" <<<"$proof" | grep -oE '#[0-9]+' | tr -d '#' | sort -un || true)
done <"$file"

[ "$bad" = 0 ] || { echo "claims-trackers: $bad tracker(s) of $file are not open" >&2; exit 1; }
echo "claims-trackers: $checked trackers, all open"
