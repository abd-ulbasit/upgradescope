#!/usr/bin/env bash
# Tests for hack/claims-trackers.sh (make hack-test), with a fake `gh` first on
# PATH that answers from a file of "<number> <STATE>" lines: a tracker named
# after "not true yet:", "not automated:", "not audited:" and the like must be
# open; a #N anywhere else in a row is history and is not asked about; with no
# token and no gh login the check skips, or fails when told to require it.
# Offline; needs bash.
set -euo pipefail
cd "$(dirname "$0")/.."
script="$PWD/hack/claims-trackers.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
mkdir "$work/bin"

# The fake gh: `gh issue view N [-R repo] --json state -q .state` prints the
# state from $FAKE_STATES (exit 1 when unknown, as for a missing issue);
# `gh auth status` succeeds only when FAKE_GH_LOGIN=1. Every call is logged.
cat >"$work/bin/gh" <<'GH'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_GH_LOG"
case "$1 ${2:-}" in
  "auth status") [ "${FAKE_GH_LOGIN:-0}" = 1 ]; exit ;;
  "issue view")
    n=$3
    state=$(awk -v n="$n" '$1 == n { print $2 }' "$FAKE_STATES")
    [ -n "$state" ] || { echo "GraphQL: Could not resolve to an issue or pull request with the number of $n." >&2; exit 1; }
    echo "$state" ;;
  *) echo "fake gh: unexpected $*" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"
printf '99 OPEN\n127 OPEN\n121 CLOSED\n126 CLOSED\n82 MERGED\n' >"$work/states"
export FAKE_STATES="$work/states" FAKE_GH_LOG="$work/ghlog"

# expect <name> <want-exit> <needle> <ledger> [env assignments...]
expect() {
  local name=$1 want=$2 needle=$3 ledger=$4 got=0
  shift 4
  printf '%s\n' "$ledger" >"$work/claims.md"
  : >"$work/ghlog"
  env PATH="$work/bin:$PATH" GH_TOKEN=tok CLAIMS_FILE="$work/claims.md" "$@" "$script" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

head='# Claims

| ID | Claim | Proven by |
|---|---|---|
| XX-01 | A test | `TestScore` |'

expect "open trackers pass" 0 "2 trackers" "$head
| XX-02 | Gap | not true yet: #127 |
| XX-03 | Gap | \`TestScore\`; not automated: #99 (the rest is by hand) |"
expect "a closed tracker fails, naming the row and the issue" 1 "XX-02: #121 is CLOSED" "$head
| XX-02 | Gap | not automated yet: #121 |"
expect "a merged pull request is not an open tracker" 1 "XX-02: #82 is MERGED" "$head
| XX-02 | Gap | not audited: #82 |"
expect "every marker is checked" 1 "XX-05: #126 is CLOSED" "$head
| XX-02 | A | not true yet: #99 |
| XX-03 | B | not automated: #99 |
| XX-04 | C | not audited: #99 |
| XX-05 | D | under repair: #126 |"
expect "several trackers after one marker are all checked" 1 "XX-02: #121 is CLOSED" "$head
| XX-02 | Gap | a manual run, not automated, #99, #121 |"
expect "a closed issue in a proven row's prose is history" 0 "0 trackers" "$head
| XX-02 | Fixed in #121 and #126, see (#82) | \`TestScore\` |"
expect "a tracker marker without an issue is not checked" 0 "0 trackers" "$head
| XX-02 | Gap | not audited |"
expect "an unknown issue fails" 1 "XX-02: #4040: could not read its state" "$head
| XX-02 | Gap | not true yet: #4040 |"
expect "an issue is asked about once" 0 "2 trackers" "$head
| XX-02 | A | not true yet: #127 |
| XX-03 | B | not true yet: #127 |"
if [ "$(grep -c 'issue view 127' "$work/ghlog")" = 1 ]; then
  echo "ok   an issue is asked about once (calls)" | tee -a "$work/results"
else
  echo "FAIL an issue is asked about once (calls): $(cat "$work/ghlog")" | tee -a "$work/results" >&2
fi

# No token: skip with a notice, fail when required, and use a gh login.
expect "no token and no gh login skips" 0 "skipping: no GH_TOKEN" "$head
| XX-02 | Gap | not true yet: #121 |" GH_TOKEN= GITHUB_TOKEN=
expect "no token and no gh login fails when required" 1 "no GH_TOKEN" "$head
| XX-02 | Gap | not true yet: #121 |" GH_TOKEN= GITHUB_TOKEN= CLAIMS_TRACKERS_REQUIRE=1
expect "a gh login stands in for a token" 1 "XX-02: #121 is CLOSED" "$head
| XX-02 | Gap | not true yet: #121 |" GH_TOKEN= GITHUB_TOKEN= FAKE_GH_LOGIN=1
expect "no gh skips" 0 "skipping: gh is not installed" "$head
| XX-02 | Gap | not true yet: #121 |" CLAIMS_TRACKERS_GH=no-such-gh
expect "no gh fails when required" 1 "gh is not installed" "$head
| XX-02 | Gap | not true yet: #121 |" CLAIMS_TRACKERS_GH=no-such-gh CLAIMS_TRACKERS_REQUIRE=1

# The real ledger (TestClaimsLedgerTrackersOpen): against a fake GitHub where
# every issue it mentions is open except the ones that were closed under it
# (#366: 71, 76, 121, 126, 226, 228, 229, 233, 247, 248), no tracker may be
# one of those, and the extraction must find trackers in it.
grep -oE '#[0-9]+' docs/claims.md | tr -d '#' | sort -u |
  awk 'BEGIN { n = split("71 76 121 126 226 228 229 233 247 248", c, " "); for (i = 1; i <= n; i++) closed[c[i]] = 1 }
       { print $1, ($1 in closed) ? "CLOSED" : "OPEN" }' >"$work/states"
got=0
PATH="$work/bin:$PATH" GH_TOKEN=tok "$script" >"$work/out" 2>&1 || got=$?
if [ "$got" = 0 ] && grep -qE '[1-9][0-9]* trackers' "$work/out"; then
  echo "ok   docs/claims.md: $(tail -1 "$work/out")" | tee -a "$work/results"
else
  echo "FAIL docs/claims.md:" >&2; sed 's/^/     /' "$work/out" >&2
  echo "FAIL docs/claims.md" >>"$work/results"
fi
if grep -q 'hack/claims-trackers.sh' .github/workflows/ci.yml && grep -q 'claims-trackers_test.sh' Makefile; then
  echo "ok   CI runs the check and make hack-test runs this test" | tee -a "$work/results"
else
  echo "FAIL the check is not wired into ci.yml and the Makefile" | tee -a "$work/results" >&2
fi

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "claims-trackers_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
