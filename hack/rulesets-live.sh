#!/usr/bin/env bash
# Checks that the rulesets applied on GitHub equal the files in
# .github/rulesets/ (make rulesets-check; CI's repo-checks job on pull
# requests and pushes, not on a release). GitHub does not read those files, so
# a ruleset edited in the UI, or a file changed and never applied, would
# otherwise go unseen: IR-06 (the `breaking-change` check is required on
# main) and IR-11 (release tags cannot be moved or deleted) are claims about
# the live rulesets.
#
# Read-only: it only GETs `repos/$REPO/rulesets` and `.../rulesets/<id>`
# (metadata read, which the job's github.token has, and which works
# anonymously on a public repository). It never changes a ruleset.
#
# The applied files are listed below, by name: major-tag.json is not applied
# (see .github/rulesets/README.md) and is never compared. For each file the
# live ruleset of the same `name` must exist and equal it in `target`,
# `enforcement`, `conditions` and `rules`. The rules are compared as a set
# (sorted by type, then canonical JSON), their parameters in full with sorted
# keys, and `required_status_checks` as a set, so the order the API returns
# things in does not matter, and a dropped `integration_id` does.
# `bypass_actors` is compared only when the live answer has the field: GitHub
# shows it to admins only, so the job's token does not get it; one line says
# when it was not compared.
#
# A mismatch prints a diff naming the file and the field, and the script exits
# 1. A GET that fails is retried twice; if the API still cannot be read it
# fails closed (it never skips).
#
# Knobs: RULESETS_DIR (default .github/rulesets), RULESETS_REPO (default
# $GITHUB_REPOSITORY, else abd-ulbasit/upgradescope), RULESETS_GH (the gh
# command), RULESETS_RETRY_SLEEP (seconds between attempts, default 2).
set -euo pipefail
cd "$(dirname "$0")/.."

dir=${RULESETS_DIR:-.github/rulesets}
repo=${RULESETS_REPO:-${GITHUB_REPOSITORY:-abd-ulbasit/upgradescope}}
gh_cmd=${RULESETS_GH:-gh}
sleep_s=${RULESETS_RETRY_SLEEP:-2}
applied=(main.json release-tags.json)

command -v jq >/dev/null 2>&1 || { echo "rulesets-live: jq is not installed" >&2; exit 1; }
command -v "$gh_cmd" >/dev/null 2>&1 || { echo "rulesets-live: gh is not installed, so the live rulesets cannot be read" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# api <path> <outfile>: GET, retried twice; fails closed.
api() {
  local path=$1 out=$2 attempt
  for attempt in 1 2 3; do
    if "$gh_cmd" api "$path" >"$out" 2>"$work/err"; then return 0; fi
    [ "$attempt" = 3 ] || sleep "$sleep_s"
  done
  echo "rulesets-live: could not read $path ($repo): $(tr '\n' ' ' <"$work/err")" >&2
  return 1
}

# The comparable form of a ruleset (a file or the API's answer). Keys are
# sorted by jq -S; sets are sorted; an absent parameters, include or exclude
# is the same as an empty one. bypass_actors is kept only if present.
norm='
  def set: sort_by(tostring);
  def ref: (. // {}) | {include: ((.include // []) | sort), exclude: ((.exclude // []) | sort)};
  def rule:
    if .parameters == null then del(.parameters)
    else .parameters |= (
      (if type == "object" and has("required_status_checks")
         then .required_status_checks |= set else . end)
      | (if type == "object" and has("allowed_merge_methods")
         then .allowed_merge_methods |= sort else . end)) end;
  { target, enforcement,
    conditions: ((.conditions // {}) | if has("ref_name") then .ref_name |= ref else . end),
    rules: ([.rules[]? | rule] | sort_by(.type, tostring)) }
  + (if has("bypass_actors") then {bypass_actors: (.bypass_actors | set)} else {} end)'

api "repos/$repo/rulesets?per_page=100" "$work/list.json" || exit 1
jq -e 'type == "array"' "$work/list.json" >/dev/null 2>&1 ||
  { echo "rulesets-live: the list of rulesets of $repo is not a JSON array: $(head -c 300 "$work/list.json")" >&2; exit 1; }

bad=0 matched=0 compared_bypass=0 skipped_bypass=0
for f in "${applied[@]}"; do
  file="$dir/$f"
  if ! jq -e . "$file" >/dev/null 2>&1; then
    echo "rulesets-live: $f: cannot read $file as JSON" >&2
    bad=$((bad + 1))
    continue
  fi
  name=$(jq -r '.name' "$file")
  id=$(jq -r --arg n "$name" '[.[] | select(.name == $n)][0].id // empty' "$work/list.json")
  if [ -z "$id" ]; then
    echo "rulesets-live: $f: no live ruleset named \"$name\" in $repo (apply it: .github/rulesets/README.md)" >&2
    bad=$((bad + 1))
    continue
  fi
  api "repos/$repo/rulesets/$id" "$work/live-$id.json" || exit 1
  jq -e 'type == "object"' "$work/live-$id.json" >/dev/null 2>&1 ||
    { echo "rulesets-live: could not read rulesets/$id ($repo): not a JSON object" >&2; exit 1; }

  jq -S "$norm" "$file" >"$work/want.json"
  jq -S "$norm" "$work/live-$id.json" >"$work/got.json"
  if jq -e 'has("bypass_actors")' "$work/got.json" >/dev/null; then
    compared_bypass=$((compared_bypass + 1))
  else
    skipped_bypass=$((skipped_bypass + 1))
    jq 'del(.bypass_actors)' "$work/want.json" >"$work/w.t" && mv "$work/w.t" "$work/want.json"
  fi

  ok=1
  for field in target enforcement conditions rules bypass_actors; do
    jq -S --arg f "$field" '{($f): .[$f]}' "$work/want.json" >"$work/want-$field.json"
    jq -S --arg f "$field" '{($f): .[$f]}' "$work/got.json" >"$work/got-$field.json"
    if ! cmp -s "$work/want-$field.json" "$work/got-$field.json"; then
      ok=0
      {
        echo "rulesets-live: $f: $field differs from live ruleset $id (\"$name\"); - is the file, + is live:"
        diff -u -L "$f" -L "live ruleset $id" "$work/want-$field.json" "$work/got-$field.json" | tail -n +3 || true
      } >&2
    fi
  done
  if [ "$ok" = 1 ]; then matched=$((matched + 1)); else bad=$((bad + 1)); fi
done

if [ "$skipped_bypass" -gt 0 ]; then
  echo "rulesets-live: bypass_actors not compared for $skipped_bypass ruleset(s): the live answer does not include it (only repository admins see it)"
fi
if [ "$compared_bypass" -gt 0 ]; then
  echo "rulesets-live: bypass_actors compared for $compared_bypass ruleset(s)"
fi
if [ "$bad" -gt 0 ]; then
  echo "rulesets-live: $bad of ${#applied[@]} applied ruleset files of $dir differ from $repo; re-apply the file, or update it after an intended change (.github/rulesets/README.md)" >&2
  exit 1
fi
echo "rulesets-live: $repo: ${#applied[@]} rulesets match the files in $dir ($matched checked)"
