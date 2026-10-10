#!/usr/bin/env bash
# Tests for hack/renovate-audit.sh (make hack-test), against npm audit
# --json reports and ignore lists written here: the gate must fail on a new
# advisory, an expired entry, an entry whose package gained a fix, a broken
# ignore list and a broken audit, and warn on a stale entry. Also checks
# that CI runs the gate on the real lockfile. Offline; needs jq.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}

# vuln <pkg> <GHSA id> <fixAvailable JSON>: one vulnerabilities entry.
vuln() {
  jq -nc --arg p "$1" --arg id "$2" --argjson fix "$3" \
    '{($p): {name: $p, severity: "high", via: [{source: 1, name: $p, url: ("https://github.com/advisories/" + $id), range: "*"}], fixAvailable: $fix}}'
}
# report <file> <vuln JSON>...: an npm audit --json report.
report() {
  local f=$1
  shift
  { [ $# -gt 0 ] && printf '%s\n' "$@" || echo '{}'; } | jq -cs '{auditReportVersion: 2, vulnerabilities: (add // {}),
    metadata: {vulnerabilities: {info: 0, low: 0, moderate: 0, high: (add // {} | length), critical: 0, total: (add // {} | length)}}}' >"$f"
}
major='{"name":"renovate","version":"21.33.15","isSemVerMajor":true}'
report "$work/two.json" "$(vuln braces GHSA-vfj7-8cjw-p6xm "$major")" "$(vuln sprintf-js GHSA-hp3w-g68c-fv3c "$major")" \
  '{"micromatch":{"name":"micromatch","severity":"high","via":["braces"],"fixAvailable":'"$major"'}}'
report "$work/new.json" "$(vuln braces GHSA-vfj7-8cjw-p6xm "$major")" "$(vuln handlebars GHSA-xw65-4hp5-5hc7 "$major")"
report "$work/fixable.json" "$(vuln braces GHSA-vfj7-8cjw-p6xm true)"
report "$work/minor.json" "$(vuln braces GHSA-vfj7-8cjw-p6xm '{"name":"renovate","version":"44.200.0","isSemVerMajor":false}')"
report "$work/one.json" "$(vuln braces GHSA-vfj7-8cjw-p6xm "$major")"
report "$work/none.json"
echo 'npm error code ENOTFOUND' >"$work/broken.json"
echo '{"error":{"code":"ENOLOCK"}}' >"$work/nolock.json"

cat >"$work/ignore.toml" <<'EOF'
# comment

[[IgnoredVulns]]
id = "GHSA-vfj7-8cjw-p6xm"
ignoreUntil = 2027-01-09
reason = "no patched release"

[[IgnoredVulns]]
id = "GHSA-hp3w-g68c-fv3c"
ignoreUntil = 2027-01-09
reason = "no patched release either"
EOF

# expect <name> <want-exit> <needle> <report> [ignore] [today]
expect() {
  local name=$1 want=$2 needle=$3 got=0
  RENOVATE_AUDIT_JSON=$4 RENOVATE_AUDIT_IGNORE=${5:-$work/ignore.toml} RENOVATE_AUDIT_TODAY=${6:-2026-10-09} \
    hack/renovate-audit.sh >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then ok "$name"; else
    echo "(exit $got, want $want and '$needle')" >>"$work/out"
    fail "$name" "$work/out"
  fi
}
expect "accepted advisories pass" 0 "GHSA-vfj7-8cjw-p6xm (braces) accepted until 2027-01-09" "$work/two.json"
expect "the summary counts advisories and entries" 0 "3 vulnerable packages reported, 2 advisories, 2 accepted" "$work/two.json"
expect "a new advisory fails" 1 "::error::renovate-audit: GHSA-xw65-4hp5-5hc7 in handlebars (hack/renovate/package-lock.json) is not accepted" "$work/new.json"
expect "an entry past its ignoreUntil fails" 1 "expired on 2027-01-09" "$work/one.json" "" 2027-01-10
expect "an entry is still valid on its ignoreUntil day" 0 "accepted until 2027-01-09" "$work/one.json" "" 2027-01-09
expect "an accepted advisory that npm can now fix fails" 1 "now has a fix the lockfile can take" "$work/fixable.json"
expect "an accepted advisory a newer Renovate minor fixes fails" 1 "now has a fix the lockfile can take" "$work/minor.json"
expect "an entry no advisory matches warns" 0 "::warning::renovate-audit: the $work/ignore.toml entry for GHSA-hp3w-g68c-fv3c matches no advisory any more" "$work/one.json"
expect "no advisories pass" 0 "0 vulnerable packages reported" "$work/none.json"
expect "an audit that printed no report fails closed" 2 "npm audit did not produce a report" "$work/broken.json"
expect "an npm error report fails closed" 2 "npm audit did not produce a report" "$work/nolock.json"
# bad_ignore <name> <toml> <needle>
bad_ignore() {
  printf '%s\n' "$2" >"$work/bad.toml"
  expect "$1" 2 "$3" "$work/two.json" "$work/bad.toml"
}
bad_ignore "an entry without a reason is refused" '[[IgnoredVulns]]
id = "GHSA-vfj7-8cjw-p6xm"
ignoreUntil = 2027-01-09' "needs id, ignoreUntil and reason"
bad_ignore "an entry without ignoreUntil is refused" '[[IgnoredVulns]]
id = "GHSA-vfj7-8cjw-p6xm"
reason = "x"' "needs id, ignoreUntil and reason"
bad_ignore "a quoted or malformed date is refused" '[[IgnoredVulns]]
id = "GHSA-vfj7-8cjw-p6xm"
ignoreUntil = "2027-01-09"
reason = "x"' "cannot read line 3"
bad_ignore "an ID that is not a GHSA ID is refused" '[[IgnoredVulns]]
id = "CVE-2024-4068"
ignoreUntil = 2027-01-09
reason = "x"' "cannot read line 2"
bad_ignore "another table is refused" '[[PackageOverrides]]
name = "braces"' "unexpected table"
expect "a missing ignore list fails" 2 "is missing" "$work/two.json" "$work/no-such.toml"

# The real ignore list parses and every entry has a reason; CI runs the gate.
if RENOVATE_AUDIT_JSON="$work/none.json" hack/renovate-audit.sh >"$work/out" 2>&1; then
  ok "hack/renovate/osv-scanner.toml is a valid ignore list"
else
  fail "hack/renovate/osv-scanner.toml is a valid ignore list" "$work/out"
fi
job=$(awk '$0 == "  vuln:" { on = 1; next } on && /^  [a-z0-9_-]+:/ { exit } on' .github/workflows/ci.yml)
grep -qE '^        run: make renovate-audit$' <<<"$job" && grep -q 'uses: actions/setup-node@' <<<"$job" &&
  ok "CI's vuln job (which runs on the schedule too) runs make renovate-audit" ||
  fail "CI's vuln job does not run make renovate-audit with setup-node"
grep -qE '^renovate-audit:' Makefile && ok "make renovate-audit exists" || fail "no renovate-audit target in the Makefile"

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "renovate-audit_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
