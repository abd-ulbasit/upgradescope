#!/usr/bin/env bash
# Tests for hack/release-environment-live.sh (make hack-test), with a fake
# `gh` first on PATH that serves a "live" `release` environment and its
# deployment policies from fixture files, shaped as the API returns them,
# then changed one way at a time. The check passes for an environment with a
# required reviewer that deploys only tags v*.*.*, and fails when the
# environment is missing, has no reviewer (no rule, or a rule naming
# nobody), deploys from any ref or from protected branches, or selects
# another pattern, a branch or a second ref; transient API failures are
# retried twice and anything else fails closed. Also pins that ci.yml runs
# the check and make hack-test runs this test. Offline; needs bash and jq.
set -euo pipefail
cd "$(dirname "$0")/.."
script="$PWD/hack/release-environment-live.sh"
command -v jq >/dev/null || { echo "release-environment-live_test: jq is required" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
mkdir "$work/bin"

# The fake gh: `gh api repos/R/environments/release` prints $FAKE_LIVE/env.json,
# `.../deployment-branch-policies[?...]` prints $FAKE_LIVE/policies.json, and
# a missing file is a 404. The first $FAKE_FAIL_FIRST calls fail, as a
# transient API error would; every call is logged.
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
  repos/*/environments/release) f="$FAKE_LIVE/env.json" ;;
  repos/*/environments/release/deployment-branch-policies | repos/*/environments/release/deployment-branch-policies\?*)
    f="$FAKE_LIVE/policies.json" ;;
  *) echo "fake gh: unexpected $*" >&2; exit 2 ;;
esac
[ -f "$f" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
cat "$f"
GH
chmod +x "$work/bin/gh"
export FAKE_GH_LOG="$work/ghlog"

# fresh: an environment as it should be, in the API's shape.
fresh() {
  rm -rf "$work/live"
  mkdir "$work/live"
  cat >"$work/live/env.json" <<'EOF'
{
  "id": 1, "node_id": "EN_x", "name": "release",
  "url": "https://api.github.com/repos/o/r/environments/release",
  "created_at": "2026-10-24T00:00:00Z", "updated_at": "2026-10-24T00:00:00Z",
  "can_admins_bypass": true,
  "protection_rules": [
    { "id": 2, "node_id": "GA_x", "type": "branch_policy" },
    { "id": 3, "node_id": "GA_y", "prevent_self_review": false, "type": "required_reviewers",
      "reviewers": [ { "type": "User", "reviewer": { "login": "abd-ulbasit", "id": 4 } } ] }
  ],
  "deployment_branch_policy": { "protected_branches": false, "custom_branch_policies": true }
}
EOF
  cat >"$work/live/policies.json" <<'EOF'
{ "total_count": 1, "branch_policies": [ { "id": 5, "node_id": "GBP_x", "name": "v*.*.*", "type": "tag" } ] }
EOF
}
# edit <env|policies> <jq filter>: change one live answer.
edit() { jq "$2" "$work/live/$1.json" >"$work/live/$1.t" && mv "$work/live/$1.t" "$work/live/$1.json"; }

# expect <name> <want-exit> <needle> [env assignments...]
expect() {
  local name=$1 want=$2 needle=$3 got=0
  shift 3
  rm -f "$work/live/.calls"
  : >"$work/ghlog"
  env PATH="$work/bin:$PATH" FAKE_LIVE="$work/live" RELEASE_ENV_RETRY_SLEEP=0 "$@" "$script" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

fresh
expect "a reviewer and only the tag pattern v*.*.* pass" 0 'requires a reviewer (1 named) and deploys only tags v*.*.*'
fresh
edit env '.protection_rules[1].reviewers += [{type: "Team", reviewer: {slug: "maintainers", id: 6}}]'
expect "two reviewers pass" 0 '(2 named)'

# What must fail.
fresh
rm "$work/live/env.json"
expect "no release environment fails (GitHub would create it unprotected)" 1 'has no environment named "release"'
[ "$(grep -c 'environments/release$' "$work/ghlog")" = 1 ] &&
  echo "ok   a 404 for the environment is not retried" | tee -a "$work/results" ||
  { echo "FAIL a 404 for the environment is not retried: $(cat "$work/ghlog")" | tee -a "$work/results" >&2; }
fresh
edit env '.protection_rules |= map(select(.type != "required_reviewers"))'
expect "no required_reviewers rule fails" 1 'has no required reviewer'
fresh
edit env '.protection_rules[1].reviewers = []'
expect "a required_reviewers rule naming nobody fails" 1 'has no required reviewer'
fresh
edit env '.protection_rules = [{type: "wait_timer", wait_timer: 30}]'
expect "a wait timer alone is not a reviewer" 1 'has no required reviewer'
fresh
edit env '.deployment_branch_policy = null'
expect "deployments from any ref fail" 1 'does not limit deployments to selected tags'
fresh
edit env '.deployment_branch_policy = {protected_branches: true, custom_branch_policies: false}'
expect "deployments from protected branches fail" 1 'does not limit deployments to selected tags'
fresh
edit policies '.branch_policies[0].name = "v*"'
expect "another tag pattern fails" 1 'want only the tag pattern'
fresh
edit policies '.branch_policies[0].type = "branch"'
expect "a branch pattern named v*.*.* fails" 1 'want only the tag pattern'
fresh
edit policies '.branch_policies += [{id: 7, name: "main", type: "branch"}] | .total_count = 2'
expect "an extra branch beside the tag pattern fails" 1 'want only the tag pattern'
fresh
edit policies '.branch_policies = [] | .total_count = 0'
expect "selected refs with none selected fail" 1 'want only the tag pattern'
fresh
edit env '.protection_rules[1].reviewers = []'
edit env '.deployment_branch_policy = null'
expect "every problem is reported, then a count" 1 '2 problem(s)'

# The API: transient failures are retried twice; anything else fails closed.
fresh
expect "two transient API failures are retried" 0 'requires a reviewer' FAKE_FAIL_FIRST=2
fresh
expect "three API failures in a row fail closed" 1 'could not read' FAKE_FAIL_FIRST=3
fresh
echo 'not json' >"$work/live/env.json"
expect "an answer that is not JSON fails closed" 1 'is not a JSON object with protection_rules'
fresh
echo '{"message":"Resource not accessible by integration"}' >"$work/live/env.json"
expect "an error object fails closed" 1 'is not a JSON object with protection_rules'
fresh
rm "$work/live/policies.json"
expect "unreadable deployment policies fail closed" 1 'could not read the deployment policies'
fresh
echo '{"message":"x"}' >"$work/live/policies.json"
expect "deployment policies that are not a list fail closed" 1 'are not a list'
fresh
expect "no gh fails closed" 1 'gh is not installed' RELEASE_ENV_GH=no-such-gh

# The repository is a knob.
fresh
expect "the repository is a knob" 0 'other/repo' RELEASE_ENV_REPO=other/repo
if grep -q 'repos/other/repo/environments/release' "$work/ghlog"; then
  echo "ok   the repository is read from RELEASE_ENV_REPO" | tee -a "$work/results"
else
  echo "FAIL the repository is read from RELEASE_ENV_REPO: $(cat "$work/ghlog")" | tee -a "$work/results" >&2
fi

# Wiring: the Makefile and ci.yml. The step's token can read environments
# only with actions: read, and release.yml's ci job grants whatever ci.yml
# asks for (hack/release-ci-permissions_test.sh).
if grep -q 'release-environment-live_test.sh' Makefile && grep -qE '^release-environment-check:' Makefile; then
  echo "ok   make hack-test runs this test, and make release-environment-check exists" | tee -a "$work/results"
else
  echo "FAIL the Makefile has no release-environment-live_test.sh in hack-test or no release-environment-check target" | tee -a "$work/results" >&2
fi
rc=$(awk '/^  repo-checks:$/ { j = 1; next } j && /^  [a-z0-9_-]+:[ ]*$/ { j = 0 } j' .github/workflows/ci.yml)
step=$(awk '/^      - / { s = ($0 == "      - name: the release environment holds releases for a reviewer") } s' <<<"$rc")
if grep -qF 'run: hack/release-environment-live.sh' <<<"$step" && grep -qF 'GH_TOKEN: ${{ github.token }}' <<<"$step" &&
  grep -qF 'if: ${{ !inputs.release }}' <<<"$step"; then
  echo "ok   repo-checks runs hack/release-environment-live.sh with the job's token, not on a release" | tee -a "$work/results"
else
  echo "FAIL ci.yml's repo-checks job has no (or a wrong) release-environment step: $step" | tee -a "$work/results" >&2
fi
if awk '/^    permissions:/ { p = 1; next } p && !/^      / { p = 0 } p' <<<"$rc" | grep -qE '^      actions: (read|write)'; then
  echo "ok   repo-checks' token has actions: read" | tee -a "$work/results"
else
  echo "FAIL repo-checks' permissions lack actions: read, which reading an environment needs" | tee -a "$work/results" >&2
fi
# (The step's `if` is evaluated for a release, a pull request and a push by
# hack/claims-trackers_test.sh, which covers every online step of the job.)

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "release-environment-live_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
