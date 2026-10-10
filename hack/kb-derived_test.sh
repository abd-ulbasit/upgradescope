#!/usr/bin/env bash
# Tests hack/kb-derived.sh (make kb-derived) and its use in kb-refresh.yml
# (make hack-test), offline, against a stub go (the build jobs live in
# kb-refresh-build.yml, the PR jobs in kb-refresh.yml; #273):
#  - it runs each pinning test with -update, anchored to that one test, and
#    each test it names exists in the package it names;
#  - kb-refresh-build.yml's registry job runs `make kb-derived` after
#    `make eol-sync` and before it packages its patch, and kb-refresh.yml's
#    PR step's add-paths
#    commit every derived file the registry feeds, so the bot PR carries the
#    regenerated doc with the data (#252).
set -euo pipefail
cd "$(dirname "$0")/.."

kb=.github/workflows/kb-refresh.yml
kbb=.github/workflows/kb-refresh-build.yml
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


# Each pair of jobs that changes the data (a read-only job in kb-refresh-build.yml
# that regenerates and hands over a patch, and a -pr job that applies the
# patch with the write token and runs no repository code): `make kb-derived`
# runs in the first, between the step that changes the data and the one
# that packages the patch; the patch and the PR carry every derived file,
# and the -pr job runs no make.
check_job() { # job, the step that changes the data, the data path, derived files...
  local job=$1 step=$2 data=$3; shift 3
  jobtext() { awk -v j="  $2:" '$0 == j { c = 1; print; next } c && /^  [^ ]/ { exit } c' "$1"; }
  jobtext "$kbb" "$job" >"$work/$job"
  jobtext "$kb" "$job-pr" >"$work/$job-pr"
  line() { { grep -n -- "$2" "$work/$1" || true; } | head -1 | cut -d: -f1; }
  local sync derive pack
  sync=$(line "$job" "run: $step")
  derive=$(line "$job" 'run: make kb-derived')
  pack=$(line "$job" 'git add -A --')
  if [ -n "$sync" ] && [ -n "$derive" ] && [ -n "$pack" ] && [ "$sync" -lt "$derive" ] && [ "$derive" -lt "$pack" ]; then
    ok "the $job job runs make kb-derived between $step and packaging its patch"
  else
    fail "the $job job does not run make kb-derived after $step and before packaging (lines ${sync:-none}, ${derive:-none}, ${pack:-none})"
  fi
  if grep -q 'run: make' "$work/$job-pr"; then
    fail "the $job-pr job runs make with the write token"
  else
    ok "the $job-pr job runs no make"
  fi
  # add-paths: the block under `add-paths: |`, or its one-line value.
  awk '/^          add-paths:/ { v = $0; sub(/^ *add-paths: */, "", v); if (v != "|") { print v; exit } a = 1; next }
    a { if ($0 !~ /^            /) exit; sub(/^ +/, ""); print }' "$work/$job-pr" >"$work/$job.add-paths"
  local file
  for file in "$data" "$@"; do
    grep -qxF "$file" "$work/$job.add-paths" && ok "the $job PR commits $file" ||
      fail "the $job-pr job's add-paths lack $file (have: $(paste -sd' ' "$work/$job.add-paths"))"
    grep -qF -- "git add -A -- " "$work/$job" && grep -F -- "git add -A -- " "$work/$job" | grep -qF -- "$file" ||
      fail "the $job job's patch does not package $file"
    grep -F 'case "$path" in' -A2 "$work/$job-pr" | grep -qF -- "$file" ||
      fail "the $job-pr job's path allowlist lacks $file"
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
