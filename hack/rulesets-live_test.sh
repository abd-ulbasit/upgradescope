#!/usr/bin/env bash
# Tests for hack/rulesets-live.sh (make hack-test), with a fake `gh` first on
# PATH that serves a "live" copy of the applied rulesets from a directory of
# fixtures: the repository's own .github/rulesets/main.json and
# release-tags.json, dressed as the API returns them (ids, timestamps, rules
# and checks in another order), then changed one way at a time. The check
# passes when the live rulesets equal the files, and fails on a changed
# include pattern, a missing ruleset, an extra or a missing rule, a required
# check that lost its integration_id, a changed bypass actor (when the field is
# present), and when the API cannot be read. It passes when bypass_actors is
# absent, as it is for a token that is not an admin's. Also pins that ci.yml
# runs the check and make hack-test runs this test.
# Offline; needs bash and jq.
set -euo pipefail
cd "$(dirname "$0")/.."
script="$PWD/hack/rulesets-live.sh"
command -v jq >/dev/null || { echo "rulesets-live_test: jq is required" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
mkdir "$work/bin"

# The fake gh: `gh api repos/R/rulesets[?...]` prints $FAKE_LIVE/list.json;
# `gh api repos/R/rulesets/ID` prints $FAKE_LIVE/ID.json (exit 1 with a 404 when
# there is none). The first $FAKE_FAIL_FIRST calls fail, as a transient API
# error would; every call is logged.
cat >"$work/bin/gh" <<'GH'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_GH_LOG"
n=$(cat "$FAKE_LIVE/.calls" 2>/dev/null || echo 0)
echo $((n + 1)) >"$FAKE_LIVE/.calls"
if [ "$n" -lt "${FAKE_FAIL_FIRST:-0}" ]; then
  echo "gh: HTTP 502: Bad Gateway" >&2
  exit 1
fi
[ "$1" = api ] || { echo "fake gh: unexpected $*" >&2; exit 2; }
case "$2" in
  repos/*/rulesets | repos/*/rulesets\?*) f="$FAKE_LIVE/list.json" ;;
  repos/*/rulesets/*) f="$FAKE_LIVE/${2##*/}.json" ;;
  *) echo "fake gh: unexpected $*" >&2; exit 2 ;;
esac
[ -f "$f" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
cat "$f"
GH
chmod +x "$work/bin/gh"
export FAKE_GH_LOG="$work/ghlog"

repo_dir=.github/rulesets

# dress <file> <id>: the file as the API returns it: ids, source and
# timestamps added, the rules, the required checks and the bypass actors in
# another order and with their keys in another order, and bypass_actors kept
# (an admin's view).
dress() {
  jq --argjson id "$2" '
    . + {id: $id, node_id: "RRS_x", source_type: "Repository", source: "abd-ulbasit/upgradescope",
         created_at: "2026-10-11T00:00:00.000+05:00", updated_at: "2026-10-11T00:00:00.000+05:00",
         current_user_can_bypass: "never", _links: {self: {href: "x"}}}
    | .rules |= (reverse | map(if .parameters.required_status_checks
        then .parameters.required_status_checks |= (reverse | map(to_entries | reverse | from_entries)) else . end))
    | if has("bypass_actors") then .bypass_actors |= (reverse | map(to_entries | reverse | from_entries)) else . end
  ' "$1"
}

# fresh [-bypass]: a live directory equal to the files (without bypass_actors
# when asked, as a token that is not an admin's sees it).
fresh() {
  rm -rf "$work/live"
  mkdir "$work/live"
  dress "$repo_dir/main.json" 111 >"$work/live/111.json"
  dress "$repo_dir/release-tags.json" 222 >"$work/live/222.json"
  if [ "${1:-}" = -nobypass ]; then
    for f in "$work"/live/[0-9]*.json; do jq 'del(.bypass_actors)' "$f" >"$f.t" && mv "$f.t" "$f"; done
  fi
  relist
}
# relist: the list endpoint's answer, from the files there (it carries no rules).
relist() {
  jq -s '[.[] | {id, name, target, enforcement}]' "$work"/live/[0-9]*.json >"$work/live/list.json"
}
# edit <id> <jq filter>: change one live ruleset.
edit() {
  jq "$2" "$work/live/$1.json" >"$work/live/$1.t" && mv "$work/live/$1.t" "$work/live/$1.json"
  relist
}

# expect <name> <want-exit> <needle> [env assignments...]
expect() {
  local name=$1 want=$2 needle=$3 got=0
  shift 3
  rm -f "$work/live/.calls"
  : >"$work/ghlog"
  env PATH="$work/bin:$PATH" FAKE_LIVE="$work/live" RULESETS_RETRY_SLEEP=0 "$@" "$script" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

fresh
expect "equal rulesets pass (order and extra API fields do not matter)" 0 "2 rulesets match" 
expect "the bypass actors were compared" 0 "bypass_actors compared"
grep -q 'rulesets/111' "$work/ghlog" && grep -q 'rulesets/222' "$work/ghlog" &&
  echo "ok   each ruleset is read by id" | tee -a "$work/results" ||
  { echo "FAIL each ruleset is read by id: $(cat "$work/ghlog")" | tee -a "$work/results" >&2; }

# A ruleset that is not an applied file is never compared (major-tag.json is
# not applied), and an unrelated live ruleset is ignored.
fresh
edit 222 '.' # no-op
jq '. + [{id: 333, name: "something else", target: "branch", enforcement: "active"}]' "$work/live/list.json" >"$work/live/l.t" && mv "$work/live/l.t" "$work/live/list.json"
expect "an unrelated live ruleset is ignored" 0 "2 rulesets match"

fresh -nobypass
expect "bypass_actors absent from the live answer passes, with one line saying so" 0 "bypass_actors not compared"

# Changes that must fail, each naming the file and the field.
fresh
edit 222 '.conditions.ref_name.include = ["refs/tags/v*"]'
expect "a changed include pattern fails" 1 "release-tags.json: conditions"
fresh
edit 111 '.conditions.ref_name.include = ["refs/heads/main"]'
expect "a changed branch include fails" 1 "main.json: conditions"
fresh
edit 222 '.conditions.ref_name.exclude = ["refs/tags/v0"]'
expect "a changed exclude fails" 1 "release-tags.json: conditions"
fresh
rm "$work/live/222.json"
relist
expect "a missing ruleset fails" 1 "release-tags.json: no live ruleset named \"release tags are immutable\""
fresh
edit 222 '.rules += [{type: "creation"}]'
expect "an extra rule fails" 1 "release-tags.json: rules"
fresh
edit 222 '.rules |= map(select(.type != "update"))'
expect "a missing rule fails" 1 "release-tags.json: rules"
fresh
edit 111 '.rules |= map(if .parameters.required_status_checks then .parameters.required_status_checks |= map(del(.integration_id)) else . end)'
expect "a required check that lost its integration_id fails" 1 "main.json: rules"
fresh
edit 111 '.rules |= map(if .parameters.required_status_checks then .parameters.required_status_checks |= map(select(.context != "breaking-change")) else . end)'
expect "a required check that was dropped fails" 1 "breaking-change"
fresh
edit 111 '.rules |= map(if .type == "pull_request" then .parameters.required_approving_review_count = 1 else . end)'
expect "a changed rule parameter fails" 1 "main.json: rules"
fresh
edit 222 '.enforcement = "evaluate"'
expect "a changed enforcement fails" 1 "release-tags.json: enforcement"
fresh
edit 222 '.target = "branch"'
expect "a changed target fails" 1 "release-tags.json: target"
fresh
edit 222 '.bypass_actors = [{actor_id: 5, actor_type: "RepositoryRole", bypass_mode: "always"}]'
expect "a new bypass actor fails when the field is present" 1 "release-tags.json: bypass_actors"
fresh
edit 111 '.bypass_actors[0].actor_id = 4'
expect "a changed bypass actor fails when the field is present" 1 "main.json: bypass_actors"
fresh
edit 111 '.bypass_actors = []'
expect "a removed bypass actor fails when the field is present" 1 "main.json: bypass_actors"
fresh -nobypass
edit 222 '.rules |= map(select(.type != "update"))'
expect "a rule difference still fails with bypass_actors absent" 1 "release-tags.json: rules"
fresh
edit 222 '.rules += [{type: "creation"}]'
expect "the failure shows a diff of the file against the live ruleset" 1 "creation"

# The API: transient failures are retried twice; any other failure is closed.
fresh
expect "two transient API failures are retried" 0 "2 rulesets match" FAKE_FAIL_FIRST=2
fresh
expect "three API failures in a row fail closed" 1 "could not read" FAKE_FAIL_FIRST=3
fresh
rm "$work/live/list.json"
expect "an unreadable list fails closed" 1 "could not read"
fresh
rm "$work/live/111.json"
expect "an unreadable ruleset fails closed" 1 "could not read"
fresh
echo 'not json' >"$work/live/list.json"
expect "a list that is not JSON fails closed" 1 "not a JSON array"
fresh
echo '{"message":"Bad credentials"}' >"$work/live/list.json"
expect "a list that is an error object fails closed" 1 "not a JSON array"
fresh
expect "no gh fails closed" 1 "gh is not installed" RULESETS_GH=no-such-gh

# The directory and the repository are knobs (the proof in the PR runs the
# real check against a copy of the directory with one include changed).
fresh
mkdir "$work/copy"
cp "$repo_dir"/*.json "$work/copy/"
jq '.conditions.ref_name.include = ["refs/tags/v*"]' "$work/copy/release-tags.json" >"$work/copy/t" && mv "$work/copy/t" "$work/copy/release-tags.json"
expect "RULESETS_DIR points at another directory" 1 "release-tags.json: conditions" RULESETS_DIR="$work/copy"
fresh
expect "the repository is a knob" 0 "other/repo" RULESETS_REPO=other/repo
if grep -q 'repos/other/repo/rulesets' "$work/ghlog"; then
  echo "ok   the repository is read from RULESETS_REPO" | tee -a "$work/results"
else
  echo "FAIL the repository is read from RULESETS_REPO: $(cat "$work/ghlog")" | tee -a "$work/results" >&2
fi

# major-tag.json is not applied and is never compared.
fresh
mkdir "$work/withmajor"
cp "$repo_dir"/*.json "$work/withmajor/"
echo 'not a ruleset' >"$work/withmajor/major-tag.json"
expect "major-tag.json is not one of the applied files" 0 "2 rulesets match" RULESETS_DIR="$work/withmajor"

# A file that cannot be read is an error, not a pass.
fresh
mkdir "$work/empty"
expect "an applied file that is missing fails" 1 "main.json" RULESETS_DIR="$work/empty"

# Wiring: the Makefile and ci.yml.
if grep -q 'rulesets-live_test.sh' Makefile && grep -qE '^rulesets-check:' Makefile; then
  echo "ok   make hack-test runs this test, and make rulesets-check exists" | tee -a "$work/results"
else
  echo "FAIL the Makefile has no rulesets-live_test.sh in hack-test or no rulesets-check target" | tee -a "$work/results" >&2
fi
step=$(awk '/^  repo-checks:$/ { j = 1; next } j && /^  [a-z0-9_-]+:[ ]*$/ { j = 0 }
  j && /^      - / { s = ($0 ~ /- name: the live rulesets equal .github\/rulesets$/) }
  j && s' .github/workflows/ci.yml)
if grep -qF 'run: hack/rulesets-live.sh' <<<"$step" && grep -qF 'GH_TOKEN: ${{ github.token }}' <<<"$step" &&
  grep -qF 'if: ${{ !inputs.release }}' <<<"$step"; then
  echo "ok   repo-checks runs hack/rulesets-live.sh with the job's token, not on a release" | tee -a "$work/results"
else
  echo "FAIL ci.yml's repo-checks job has no (or a wrong) rulesets-live step: $step" | tee -a "$work/results" >&2
fi
# (The step's `if` is evaluated for a release, a pull request and a push by
# hack/claims-trackers_test.sh, which covers both online steps.)

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "rulesets-live_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
