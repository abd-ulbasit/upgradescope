#!/usr/bin/env bash
# Tests for hack/envtest.sh with a stub setup-envtest and test command, and
# for the three places that must list the same minors as its table: the
# Go test (envtestBetas), ci.yml's envtest matrix and the real table's pins.
# Offline; needs only bash.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cat >"$work/table.txt" <<'EOF'
# comment
1.24 1.24.2

1.28 1.28.3
EOF

# The stub setup-envtest records its arguments and prints a bundle path; the
# stub test records the environment it was given and reports the outcome in
# $work/outcome (the test's own PASS line, a skip, or a failure).
cat >"$work/setup" <<EOF
#!/usr/bin/env bash
echo "\$*" >>"$work/setup.calls"
echo "$work/assets-\$2"
EOF
cat >"$work/gotest" <<EOF
#!/usr/bin/env bash
echo "\$KUBEBUILDER_ASSETS \$UPGRADESCOPE_ENVTEST \$UPGRADESCOPE_ENVTEST_MINOR" >>"$work/gotest.calls"
case "\$(cat "$work/outcome")" in
  pass) echo "--- PASS: TestEnvtestMatrix (1.00s)" ;;
  skip) echo "--- SKIP: TestEnvtestMatrix (0.00s)" ;;
  none) echo "testing: warning: no tests to run" ;;
  fail) echo "--- FAIL: TestEnvtestMatrix (1.00s)"; exit 1 ;;
esac
EOF
chmod +x "$work/setup" "$work/gotest"

: >"$work/results"
# expect <name> <want-exit> <want-output-substring> <outcome> <args...>
expect() {
  local name=$1 want=$2 needle=$3 got=0
  echo "$4" >"$work/outcome"
  shift 4
  : >"$work/setup.calls" && : >"$work/gotest.calls"
  ENVTEST_VERSIONS_TABLE="${TABLE:-$work/table.txt}" ENVTEST_SETUP="$work/setup" ENVTEST_GO_TEST="$work/gotest" \
    hack/envtest.sh "$@" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}
# calls <file> <want>: what the stubs were called with, in order.
calls() {
  if [ "$(cat "$work/$1")" = "$2" ]; then
    echo "ok   $3" | tee -a "$work/results"
  else
    echo "FAIL $3: got:" >&2
    sed 's/^/     /' "$work/$1" >&2
    echo "FAIL $3" >>"$work/results"
  fi
}

index=https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/$(sed -n 's/^ENVTEST_INDEX_COMMIT=//p' hack/envtest.sh)/envtest-releases.yaml
[[ "$index" =~ /[0-9a-f]{40}/envtest-releases.yaml$ ]] || { echo "FAIL the index is not pinned to a commit: $index" >&2; exit 1; }

expect "every minor in the table runs, in order" 0 "envtest: ok (1.24 1.28)" pass
calls setup.calls "use 1.24.2 --index $index --bin-dir $PWD/bin/envtest/assets -p path
use 1.28.3 --index $index --bin-dir $PWD/bin/envtest/assets -p path" "each minor downloads its exact, pinned, verified bundle"
calls gotest.calls "$work/assets-1.24.2 1 1.24
$work/assets-1.28.3 1 1.28" "the test gets its assets, the gate and the minor it must find"
expect "one minor runs alone" 0 "envtest: ok (1.28)" pass 1.28
calls gotest.calls "$work/assets-1.28.3 1 1.28" "only that minor runs"
expect "a minor outside the table is refused" 2 "no envtest bundle for '1.30'" pass 1.30
calls gotest.calls "" "a refused minor runs nothing"
expect "a failing test fails the run" 1 "FAIL: TestEnvtestMatrix" fail 1.24
expect "a skipped test is not a pass" 1 "did not run and pass on Kubernetes 1.24" skip 1.24
expect "go test matching no test is not a pass" 1 "did not run and pass on Kubernetes 1.24" none 1.24

printf '1.24 1.24.x\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "a floating patch version is rejected" 2 "not an exact 1.24.N release" pass
printf '1.24 1.25.0\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "a bundle of another minor is rejected" 2 "not an exact 1.24.N release" pass
printf '# nothing\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "an empty table is rejected" 2 "no minors" pass

# The real table is the one source of the matrix: the Go test has a beta API
# per minor, and ci.yml runs one job leg per minor.
table_minors=$(awk '!/^#/ && NF {print $1}' hack/envtest-versions.txt | xargs)
test_minors=$(sed -n 's/^	\([0-9][0-9]*\): .*/1.\1/p' internal/collect/envtest_matrix_test.go | xargs)
ci_minors=$(awk '/^  envtest:/{j=1;next} j&&/^  [a-z]/{j=0} j&&/^ *minor: *\[/{gsub(/[^0-9.,]/,""); gsub(/,/," "); print}' .github/workflows/ci.yml | xargs)
same_minors() { # <where> <minors>
  if [ "$2" = "$table_minors" ] && [ -n "$2" ]; then
    echo "ok   $1 lists the table's minors ($2)" | tee -a "$work/results"
  else
    echo "FAIL $1 lists '$2', hack/envtest-versions.txt has '$table_minors'" | tee -a "$work/results" >&2
  fi
}
same_minors "internal/collect/envtest_matrix_test.go (envtestBetas)" "$test_minors"
same_minors "ci.yml's envtest matrix" "$ci_minors"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "envtest_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
