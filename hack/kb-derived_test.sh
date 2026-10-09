#!/usr/bin/env bash
# Tests hack/kb-derived.sh (make kb-derived) and its use in kb-refresh.yml
# (make hack-test), offline, against a stub go:
#  - it runs each pinning test with -update, anchored to that one test, and
#    each test it names exists in the package it names;
#  - kb-refresh.yml's registry job runs `make kb-derived` after
#    `make eol-sync` and before its PR step, and that step's add-paths
#    commit every derived file the registry feeds, so the bot PR carries the
#    regenerated doc with the data (#252).
set -euo pipefail
cd "$(dirname "$0")/.."

kb=.github/workflows/kb-refresh.yml
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failed=0
ok() { echo "ok   $1"; }
fail() { echo "FAIL $1" >&2; failed=1; }

mkdir -p "$work/bin"
cat >"$work/bin/go" <<'STUB'
#!/usr/bin/env bash
echo "go $*" >>"$GO_LOG"
STUB
chmod +x "$work/bin/go"

GO_LOG="$work/go.log" PATH="$work/bin:$PATH" ./hack/kb-derived.sh >"$work/out" 2>&1 || {
  cat "$work/out" >&2
  fail "kb-derived.sh failed against a stub go"
}
while IFS=$'\t' read -r pkg test; do
  if grep -qxF -- "go test $pkg -count=1 -run ^$test\$ -update" "$work/go.log"; then
    ok "runs $test in $pkg with -update"
  else
    fail "does not run 'go test $pkg -count=1 -run ^$test\$ -update' (ran: $(paste -sd';' "$work/go.log"))"
  fi
  if grep -q "^func $test(t \*testing.T)" "${pkg#./}"/*_test.go; then
    ok "$test exists in $pkg"
  else
    fail "$test is not a test of $pkg"
  fi
done <<'EOF2'
./internal/cli	TestSupportLifecycleDocExamplesMatchScanOutput
./deploy/chart	TestKBRBACRulesInSync
EOF2
[ "$(($(wc -l <"$work/go.log")))" = 2 ] && ok "runs nothing else" || fail "ran more than the two tests: $(paste -sd';' "$work/go.log")"
[ "$(($(./hack/kb-derived.sh --list | wc -l)))" = 2 ] && ok "--list names the two files" || fail "--list: $(./hack/kb-derived.sh --list | paste -sd' ' -)"
./hack/kb-derived.sh unexpected >/dev/null 2>&1 && fail "accepts an unknown argument" || ok "refuses an unknown argument"


# Each job that changes the data: `make kb-derived` between the step that
# changes it and its PR step, and every derived file the data feeds in
# that PR's add-paths.
check_job() { # job, the step that changes the data, the data path, derived files...
  local job=$1 step=$2 data=$3; shift 3
  awk -v j="  $job:" '$0 == j { c = 1; print; next } c && /^  [^ ]/ { exit } c' "$kb" >"$work/$job"
  line() { { grep -n -- "$1" "$work/$job" || true; } | head -1 | cut -d: -f1; }
  local sync derive pr
  sync=$(line "run: $step")
  derive=$(line 'run: make kb-derived')
  pr=$(line 'uses: peter-evans/create-pull-request@')
  if [ -n "$sync" ] && [ -n "$derive" ] && [ -n "$pr" ] && [ "$sync" -lt "$derive" ] && [ "$derive" -lt "$pr" ]; then
    ok "the $job job runs make kb-derived between $step and its PR"
  else
    fail "the $job job does not run make kb-derived after $step and before create-pull-request (lines ${sync:-none}, ${derive:-none}, ${pr:-none})"
  fi
  # add-paths: the block under `add-paths: |`, or its one-line value.
  awk '/^          add-paths:/ { v = $0; sub(/^ *add-paths: */, "", v); if (v != "|") { print v; exit } a = 1; next }
    a { if ($0 !~ /^            /) exit; sub(/^ +/, ""); print }' "$work/$job" >"$work/$job.add-paths"
  local file
  for file in "$data" "$@"; do
    grep -qxF "$file" "$work/$job.add-paths" && ok "the $job PR commits $file" ||
      fail "the $job job's add-paths lack $file (have: $(paste -sd' ' "$work/$job.add-paths"))"
  done
}
check_job registry 'make eol-sync' registry/data docs/concepts/support-lifecycle.md
check_job api-lifecycle 'make gen-kb' internal/kb/data docs/concepts/support-lifecycle.md deploy/chart/files/kb-rbac-rules.yaml
for file in docs/concepts/support-lifecycle.md deploy/chart/files/kb-rbac-rules.yaml; do
  ./hack/kb-derived.sh --list | grep -qxF "$file" && ok "$file is one kb-derived regenerates" ||
    fail "kb-derived.sh --list does not name $file"
done

[ "$failed" = 0 ] || { echo "kb-derived_test: FAILED" >&2; exit 1; }
echo "kb-derived_test: OK"
