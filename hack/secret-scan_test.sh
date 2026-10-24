#!/usr/bin/env bash
# Tests for hack/secret-scan.sh (make hack-test), offline, with a stub
# gitleaks in a scratch git repository. It checks that the scan
#   - passes a clean tree, scanning exactly the files git would commit
#     (tracked and untracked ones, never ignored ones), with .gitleaks.toml;
#   - fails (1) on a finding and names its file and line, redacted as
#     gitleaks reports it;
#   - fails closed (2) when gitleaks writes no report, and refuses a
#     history scan of a shallow clone;
#   - scans every ref's history with --history;
# and that CI's repo-checks job runs it.
set -euo pipefail
cd "$(dirname "$0")/.."
root=$PWD

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}

# gitleaks <dir|git> <path> ... --report-path <file>: logs its arguments and
# the files it was given, writes $STUB_LEAKS (a JSON array) as the report, and
# exits 1 when it is not empty; STUB_NO_REPORT=1 fails without a report.
cat >"$work/gitleaks" <<'EOF'
#!/usr/bin/env bash
echo "gitleaks $*" >>"$CALLS"
[ "$1" != dir ] || (cd "$2" && find . -type f | sort) >"$SCANNED"
[ "${STUB_NO_REPORT:-}" != 1 ] || { echo "gitleaks: fatal" >&2; exit 1; }
out=
while [ $# -gt 0 ]; do [ "$1" = --report-path ] && out=$2; shift; done
printf '%s\n' "${STUB_LEAKS:-[]}" >"$out"
[ "${STUB_LEAKS:-[]}" = "[]" ] || exit 1
EOF
chmod +x "$work/gitleaks"

repo="$work/repo"
mkdir -p "$repo/hack" "$repo/bin" "$repo/web/node_modules/x"
cp hack/secret-scan.sh "$repo/hack/"
cp .gitleaks.toml "$repo/"
(
  cd "$repo"
  git init -q -b main .
  printf 'bin/\nnode_modules/\n' >.gitignore
  echo 'package main' >main.go
  echo 'built' >bin/tool
  echo 'dependency' >web/node_modules/x/index.js
  git add .gitignore main.go hack .gitleaks.toml
  git -c user.name=t -c user.email=t@t commit -q -m init
  echo 'draft' >notes.txt # untracked, not ignored
)
run() {
  : >"$work/calls"
  : >"$work/scanned"
  code=0
  (cd "$repo" && env CALLS="$work/calls" SCANNED="$work/scanned" SECRET_SCAN_GITLEAKS="$work/gitleaks" "$@" \
    bash hack/secret-scan.sh ${ARGS[@]+"${ARGS[@]}"}) >"$work/out" 2>&1 || code=$?
}

ARGS=()
run
[ "$code" = 0 ] && grep -q 'no secrets in 5 files' "$work/out" && ok "a clean tree passes" || fail "a clean tree did not pass (exit $code)" "$work/out"
if grep -qx './notes.txt' "$work/scanned" && grep -qx './main.go' "$work/scanned" && grep -qx './.gitleaks.toml' "$work/scanned" &&
  ! grep -qE 'bin/tool|node_modules' "$work/scanned"; then
  ok "the files git would commit are scanned, ignored ones are not"
else
  fail "the scanned file set is wrong" "$work/scanned"
fi
grep -qF -- "--config $repo/.gitleaks.toml --redact" "$work/calls" && ok "gitleaks runs with .gitleaks.toml and --redact" || fail "gitleaks was not given the config and --redact" "$work/calls"

leak='[{"RuleID":"generic-api-key","Description":"Generic API Key","File":"main.go","StartLine":3,"Match":"key := \"REDACTED\"","Commit":""}]'
run STUB_LEAKS="$leak"
[ "$code" = 1 ] && grep -qF '::error file=main.go,line=3::secret-scan: generic-api-key: Generic API Key (key := "REDACTED")' "$work/out" &&
  grep -q 'rotate it at its source' "$work/out" && ok "a finding fails (1), names its file and line, and says to rotate" || fail "a finding did not fail with 1 (exit $code)" "$work/out"

run STUB_NO_REPORT=1
[ "$code" = 2 ] && grep -q 'did not produce a report' "$work/out" && ok "gitleaks failing without a report fails closed (2)" || fail "a gitleaks error did not exit 2 (exit $code)" "$work/out"

ARGS=(--history)
run
[ "$code" = 0 ] && grep -qF -- 'gitleaks git . --config' "$work/calls" && grep -qF -- '--log-opts=--all' "$work/calls" &&
  ok "--history scans every ref's commits" || fail "--history did not run gitleaks git --log-opts=--all (exit $code)" "$work/calls"
run STUB_LEAKS='[{"RuleID":"aws-access-token","Description":"AWS","File":"old.txt","StartLine":1,"Match":"REDACTED","Commit":"0123456789abcdef"}]'
[ "$code" = 1 ] && grep -q 'in commit 0123456789ab' "$work/out" && ok "a finding in history names its commit" || fail "a history finding did not name its commit" "$work/out"
git clone -q --depth 1 "file://$repo" "$work/shallow" 2>/dev/null
cp "$repo/hack/secret-scan.sh" "$work/shallow/hack/"
code=0
(cd "$work/shallow" && SECRET_SCAN_GITLEAKS="$work/gitleaks" CALLS="$work/calls" bash hack/secret-scan.sh --history) >"$work/out" 2>&1 || code=$?
[ "$code" = 2 ] && grep -q 'shallow clone has no history' "$work/out" && ok "--history refuses a shallow clone (2)" || fail "--history on a shallow clone did not exit 2 (exit $code)" "$work/out"

ARGS=(--bogus)
run
[ "$code" = 2 ] && ok "an unknown argument is refused (2)" || fail "an unknown argument was accepted (exit $code)" "$work/out"

# Wiring: CI's repo-checks job runs it, before anything writes into the tree.
cd "$root"
steps=$(awk '/^  repo-checks:$/ { j = 1; next } j && /^  [a-z0-9_-]+:[ ]*$/ { j = 0 } j && /^      - / { print }' .github/workflows/ci.yml)
first_run=$(grep -n -- '- run: \|- name: ' <<<"$steps" | head -1)
grep -qE 'make secret-scan' <<<"$(awk '/^  repo-checks:$/ { j = 1; next } j && /^  [a-z0-9_-]+:[ ]*$/ { j = 0 } j' .github/workflows/ci.yml)" &&
  ok "CI's repo-checks job runs make secret-scan" || fail "repo-checks does not run make secret-scan"
grep -q 'secret' <<<"$first_run" && ok "it is the job's first check" || fail "make secret-scan is not repo-checks' first check: $first_run"
grep -qE '^secret-scan:' Makefile && grep -q 'secret-scan_test.sh' Makefile && ok "make secret-scan exists and make hack-test runs this test" ||
  fail "the Makefile has no secret-scan target or does not run this test"

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "secret-scan_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
