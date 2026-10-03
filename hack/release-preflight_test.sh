#!/usr/bin/env bash
# Tests release.yml's preflight job (make hack-test). Nothing before a tag is
# pushed exercises it, and it is what stands between a stray v* tag and a
# release. This runs the tag-format and appVersion step's `run: |` block the
# way Actions does (bash -eo pipefail) against a scratch chart, and asserts
# the job fetches main's history and runs the ancestry check
# (hack/check-release-ancestry.sh, tested by its own _test.sh; #198) as well
# as the changelog check. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

wf=.github/workflows/release.yml
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() { echo "FAIL $1" >&2; echo "FAIL $1" >>"$work/results"; }

# job <name>: the lines of the top-level job <name> in release.yml.
job() { awk -v want="  $1:" '$0 == want { on = 1; next } on && /^  [a-z0-9_-]+:/ { exit } on { print }' "$wf"; }
# step_run <job> <name>: the `run: |` block of that job's step, dedented.
step_run() {
  job "$1" | awk -v want="- name: $2" '
    { line = $0; sub(/^ +/, "", line) }
    line ~ /^- / { inside = (line == want); inrun = 0; next }
    inside && line == "run: |" { match($0, /^ */); ind = RLENGTH; inrun = 1; next }
    inrun {
      match($0, /^ */)
      if ($0 !~ /^ *$/ && RLENGTH <= ind) { inrun = 0; inside = 0; next }
      print substr($0, ind + 3)
    }
  '
}

job preflight >"$work/preflight.yml"
[ -s "$work/preflight.yml" ] || { echo "FAIL cannot find the preflight job in $wf" >&2; exit 1; }

# --- the job's shape ---------------------------------------------------------
if grep -qE '^ +fetch-depth: 0( |$)' "$work/preflight.yml"; then
  ok "preflight fetches full history (origin/main's, for the ancestry check)"
else
  fail "preflight's checkout has no fetch-depth: 0, so origin/main is not there to compare with"
fi
if grep -qE '^ +run: hack/check-release-ancestry\.sh$' "$work/preflight.yml"; then
  ok "preflight runs hack/check-release-ancestry.sh"
else
  fail "preflight does not run hack/check-release-ancestry.sh"
fi
if grep -qE '^ +run: hack/check-changelog\.sh "\$GITHUB_REF_NAME"$' "$work/preflight.yml"; then
  ok "preflight still checks CHANGELOG.md"
else
  fail "preflight no longer runs hack/check-changelog.sh"
fi

# --- the tag-format and appVersion step --------------------------------------
step_run preflight 'the tag is vX.Y.Z[-pre] and the chart pulls the image it publishes' >"$work/step.sh"
[ -s "$work/step.sh" ] || { echo "FAIL cannot find the tag-format step's run block in $wf" >&2; exit 1; }

mkdir -p "$work/chart/deploy/chart"
printf 'apiVersion: v2\nname: upgradescope\nversion: 0.2.0\nappVersion: "v0.2.0"\n' >"$work/chart/deploy/chart/Chart.yaml"
run_step() { # run_step <ref-type> <tag>
  (cd "$work/chart" && : >"$work/gh-output" && REF_TYPE=$1 GITHUB_REF_NAME=$2 GITHUB_OUTPUT="$work/gh-output" \
    bash -eo pipefail "$work/step.sh") >"$work/out" 2>&1
}
expect() { # expect <name> <want-exit> <substring> <ref-type> <tag>
  local name=$1 want=$2 needle=$3 got=0
  run_step "$4" "$5" || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out" "$work/gh-output"; then
    ok "$name"
  else
    fail "$name: exit $got (want $want), output:"
    sed 's/^/     /' "$work/out" >&2
  fi
}
expect "the tag the chart names passes, and its version is output" 0 "version=0.2.0" tag v0.2.0
expect "a tag that is not vX.Y.Z is refused" 1 "is not a vMAJOR.MINOR.PATCH" tag release-1
expect "a branch ref is refused" 1 "is not a vMAJOR.MINOR.PATCH" branch v0.2.0
expect "a tag the chart does not name is refused" 1 "appVersion is 'v0.2.0' but the tag is 'v0.3.0'" tag v0.3.0

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "release-preflight_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
