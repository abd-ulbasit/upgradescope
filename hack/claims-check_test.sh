#!/usr/bin/env bash
# Tests for hack/claims-check.sh (make hack-test) on fixture ledgers that
# point at this repository: one whose every reference exists passes, each
# kind of dangling reference fails, naming it, and so does an ID given to
# more than one claim row. Offline; needs bash.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

good='# Claims

## Area

| ID | Claim | Proven by |
|---|---|---|
| XX-01 | A Go test | `TestITKubeContext` |
| XX-02 | An e2e gate, a CI job, a make target, a file | `e2e:audit_scan_writes_nothing` · `ci:kube` · `make e2e` · `hack/e2e.sh` |
| XX-03 | Not automated yet | not automated: #99 |
| XX-04 | Two tests, one in a tools module | `TestScore`, `TestComputeCycles` |'

: >"$work/results"
# expect <name> <want-exit> <needle> <ledger>: run claims-check on it.
expect() {
  local name=$1 want=$2 needle=$3 got=0
  printf '%s\n' "$4" >"$work/claims.md"
  CLAIMS_FILE="$work/claims.md" hack/claims-check.sh >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

expect "every reference exists" 0 "4 claims" "$good"
expect "a missing Go test fails" 1 "XX-05: no Go test TestNoSuchClaimTest" "$good
| XX-05 | Renamed away | \`TestNoSuchClaimTest\` |"
expect "a missing e2e gate fails" 1 "XX-05: hack/e2e.sh has no gate audit_nothing" "$good
| XX-05 | Gone | \`e2e:audit_nothing\` |"
expect "an e2e helper that is never run fails" 1 "XX-05: hack/e2e.sh has no gate gate" "$good
| XX-05 | Not a gate | \`e2e:gate\` |"
expect "a missing CI job fails" 1 "XX-05: .github/workflows/ci.yml has no job nightly" "$good
| XX-05 | Gone | \`ci:nightly\` |"
expect "a missing make target fails" 1 "XX-05: the Makefile has no target claims-nope" "$good
| XX-05 | Gone | \`make claims-nope\` |"
expect "a missing file fails" 1 "XX-05: no file hack/no-such-script.sh" "$good
| XX-05 | Gone | \`hack/no-such-script.sh\` |"
expect "an unknown reference form fails" 1 "XX-05: unknown reference 'go test ./...'" "$good
| XX-05 | Vague | \`go test ./...\` |"
expect "a claim with neither a test nor an issue fails" 1 "XX-05: names no test and no tracking issue" "$good
| XX-05 | Trust me | soon |"
# A claim ID names one claim: two rows with the same ID fail, wherever they
# are (here in another table, as a claim and its row under repair would be),
# and so does an ID one row lists among others or inside a range. A suffixed
# ID (XX-01b) is another ID.
expect "two rows with one ID fail" 1 "XX-02: the ID of more than one claim row" "$good

## Under repair

| ID | Claim | Proven by |
|---|---|---|
| XX-02 | Another claim given the same ID | not true yet: #127 |"
expect "an ID among several in a row fails" 1 "XX-03: the ID of more than one claim row" "$good
| XX-05, XX-03 | Two IDs, one taken | not true yet: #127 |"
expect "an ID inside a range fails" 1 "XX-10: the ID of more than one claim row" "$good
| XX-10 | A claim | not true yet: #127 |
| XX-03b, XX-09 to XX-11 | A range over XX-10 | not true yet: #127 |"
expect "distinct IDs, suffixed and in ranges, pass" 0 "7 claims" "$good
| XX-01b | A suffixed ID is another claim | not true yet: #127 |
| XX-05 to XX-07, XX-08 | A range and a list, all new | not true yet: #127 |
| XX-09 | Nothing shared | not true yet: #127 |"
expect "a ledger with no claim rows fails" 1 "no claim rows" "# Claims

Nothing here."

# A test that exists only in a git-ignored copy (node_modules, or another
# branch's worktree under .claude/) is not in the repository: CI would not
# find it either. The "_" directory keeps go ./... away from the fixture.
stray=web/node_modules/_claims-check-fixture
mkdir -p "$stray"
trap 'rm -rf "$work" "$stray"; rmdir web/node_modules 2>/dev/null || true' EXIT
printf 'package fixture\n\nimport "testing"\n\nfunc TestOnlyInAnIgnoredCopy(t *testing.T) {}\n' >"$stray/stray_test.go"
expect "a test only in a git-ignored directory fails" 1 "XX-05: no Go test TestOnlyInAnIgnoredCopy" "$good
| XX-05 | Lives elsewhere | \`TestOnlyInAnIgnoredCopy\` |"
rm -rf "$stray"
rmdir web/node_modules 2>/dev/null || true

# The real ledger.
got=0
hack/claims-check.sh >"$work/out" 2>&1 || got=$?
if [ "$got" = 0 ]; then
  echo "ok   docs/claims.md: $(tail -1 "$work/out")" | tee -a "$work/results"
else
  echo "FAIL docs/claims.md:" >&2
  sed 's/^/     /' "$work/out" >&2
  echo "FAIL docs/claims.md" >>"$work/results"
fi

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "claims-check_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
