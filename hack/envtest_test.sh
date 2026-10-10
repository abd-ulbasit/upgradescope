#!/usr/bin/env bash
# Tests for hack/envtest.sh with a stub setup-envtest and test command, and
# for the places that must agree with its table: the Go test (envtestBetas),
# the per-event choice of minors (the pr rows on a PR, every row weekly) in
# the script and in ci.yml's kube-matrix and envtest jobs, and the real
# table's pins. Offline; needs only bash and awk.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cat >"$work/table.txt" <<'EOF'
# comment
1.24 1.24.2 pr
1.26 1.26.1 weekly

1.28 1.28.3 pr
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

expect "every minor in the table runs, in order, whatever its schedule" 0 "envtest: ok (1.24 1.26 1.28)" pass
calls setup.calls "use 1.24.2 --index $index --bin-dir $PWD/bin/envtest/assets -p path
use 1.26.1 --index $index --bin-dir $PWD/bin/envtest/assets -p path
use 1.28.3 --index $index --bin-dir $PWD/bin/envtest/assets -p path" "each minor downloads its exact, pinned, verified bundle"
calls gotest.calls "$work/assets-1.24.2 1 1.24
$work/assets-1.26.1 1 1.26
$work/assets-1.28.3 1 1.28" "the test gets its assets, the gate and the minor it must find"
expect "a weekly minor can be run on request" 0 "envtest: ok (1.26)" pass 1.26
expect "one minor runs alone" 0 "envtest: ok (1.28)" pass 1.28
calls gotest.calls "$work/assets-1.28.3 1 1.28" "only that minor runs"
expect "a minor outside the table is refused" 2 "no envtest bundle for '1.30'" pass 1.30
calls gotest.calls "" "a refused minor runs nothing"
expect "a failing test fails the run" 1 "FAIL: TestEnvtestMatrix" fail 1.24
expect "a skipped test is not a pass" 1 "did not run and pass on Kubernetes 1.24" skip 1.24
expect "go test matching no test is not a pass" 1 "did not run and pass on Kubernetes 1.24" none 1.24

# Which minors a run takes: the pr rows or every row, as JSON.
expect "the pr matrix is the rows marked pr" 0 '["1.24","1.28"]' pass matrix pr
expect "the full matrix is every row, in order" 0 '["1.24","1.26","1.28"]' pass matrix all
calls setup.calls "" "printing a matrix downloads nothing"
calls gotest.calls "" "printing a matrix runs no test"
expect "an unknown matrix set is refused" 2 "usage" pass matrix nightly
expect "a matrix with no set is refused" 2 "usage" pass matrix

printf '1.24 1.24.x pr\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "a floating patch version is rejected" 2 "not an exact 1.24.N release" pass
printf '1.24 1.25.0 pr\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "a bundle of another minor is rejected" 2 "not an exact 1.24.N release" pass
printf '1.24 1.24.2\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "a row with no schedule is rejected" 2 "must be pr or weekly" pass
printf '1.24 1.24.2 sometimes\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "a bad schedule is rejected" 2 "must be pr or weekly" pass matrix all
printf '# nothing\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "an empty table is rejected" 2 "no minors" pass

# The real table is the one source of the matrix: the Go test has a beta API
# per minor, and ci.yml reads the table for its legs (it lists no minor).
table_minors=$(awk '!/^#/ && NF {print $1}' hack/envtest-versions.txt | xargs)
table_pr=$(awk '!/^#/ && NF && $3 == "pr" {print $1}' hack/envtest-versions.txt | xargs)
test_minors=$(sed -n 's/^	\([0-9][0-9]*\): .*/1.\1/p' internal/collect/envtest_matrix_test.go | xargs)
check() { # <name> <ok|bad> <got> <want>
  if [ "$3" = "$4" ] && [ -n "$4" ]; then
    echo "ok   $1 ($3)" | tee -a "$work/results"
  else
    echo "FAIL $1: '$3', want '$4'" | tee -a "$work/results" >&2
  fi
}
json_list() { tr -d '[]"' <<<"$1" | tr ',' ' ' | xargs; }
check "every minor in the table is in internal/collect/envtest_matrix_test.go (envtestBetas)" ok "$test_minors" "$table_minors"
check "the pr matrix is exactly the rows marked pr" ok "$(json_list "$(hack/envtest.sh matrix pr)")" "$table_pr"
check "the full matrix is every row" ok "$(json_list "$(hack/envtest.sh matrix all)")" "$table_minors"
# The assertion that a warning is raised one minor before a removal needs a
# beta API removed two or more minors after the cluster's own minor: only 1.27
# (FlowSchema v1beta2, removed in 1.29). It is not on a PR, so it must stay
# in the table, where the weekly run and a dispatch take it.
case " $table_minors " in
  *" 1.27 "*) echo "ok   1.27, the only minor that asserts the warning one minor before a removal, is in the weekly set" | tee -a "$work/results" ;;
  *) echo "FAIL 1.27 is not in hack/envtest-versions.txt: nothing asserts the warning one minor before a removal" | tee -a "$work/results" >&2 ;;
esac

# The per-event choice, end to end through the two scripts ci.yml's
# kube-matrix job runs: a pull request and a push take the pr rows, the
# schedule and a dispatch take every row, a release run (even re-dispatched)
# takes the pr rows.
event_minors() { # <event> <release> <full-matrix>
  json_list "$(hack/envtest.sh matrix "$(hack/kind-images.sh set "$@")")"
}
check "a pull request runs the pr rows" ok "$(event_minors pull_request false '')" "$table_pr"
check "a push to main runs the pr rows" ok "$(event_minors push false '')" "$table_pr"
check "the schedule runs every minor" ok "$(event_minors schedule false '')" "$table_minors"
check "a dispatch runs every minor" ok "$(event_minors workflow_dispatch false true)" "$table_minors"
check "a dispatch with full-matrix=false runs the pr rows" ok "$(event_minors workflow_dispatch false false)" "$table_pr"
check "a release run runs the pr rows" ok "$(event_minors workflow_dispatch true '')" "$table_pr"

# ci.yml wires it: kube-matrix hands the script's matrix, for the set it
# chose, to the envtest job, whose matrix lists no minor of its own.
ci=.github/workflows/ci.yml
job() { awk -v j="  $2:" '$0 == j {on=1; next} on && /^  [a-z]/ {on=0} on' "$1"; }
wired() { # <ci.yml>: prints "ok" when the wiring is right, else what is wrong
  local f=$1 kube envt
  kube=$(job "$f" kube-matrix) envt=$(job "$f" envtest)
  grep -qF 'envtest-minors: ${{ steps.matrix.outputs.envtest-minors }}' <<<"$kube" || { echo "kube-matrix does not output envtest-minors"; return; }
  grep -qF 'set="$(hack/kind-images.sh set "$EVENT" "$RELEASE" "$FULL_MATRIX")"' <<<"$kube" || { echo "kube-matrix does not choose its set with hack/kind-images.sh set"; return; }
  grep -qF 'envtest="$(hack/envtest.sh matrix "$set")"' <<<"$kube" || { echo "kube-matrix does not take the envtest minors from hack/envtest.sh matrix for that set"; return; }
  grep -qF 'echo "envtest-minors=$envtest" >> "$GITHUB_OUTPUT"' <<<"$kube" || { echo "kube-matrix does not write the envtest minors"; return; }
  grep -qE '^ +minor: \$\{\{ fromJSON\(needs\.kube-matrix\.outputs\.envtest-minors\) \}\}$' <<<"$envt" || { echo "the envtest job's matrix is not from kube-matrix's envtest-minors"; return; }
  grep -qE "^ +needs: \[changes, kube-matrix\]$" <<<"$envt" || { echo "the envtest job does not need kube-matrix"; return; }
  grep -qF "needs.kube-matrix.result == 'success'" <<<"$envt" || { echo "the envtest job does not wait for kube-matrix to succeed"; return; }
  echo ok
}
if [ "$(wired "$ci")" = ok ]; then
  echo "ok   ci.yml's envtest job takes its minors from hack/envtest.sh matrix, per event" | tee -a "$work/results"
else
  echo "FAIL ci.yml: $(wired "$ci")" | tee -a "$work/results" >&2
fi

# Mutation checks: each of these breaks the per-event selection and must be
# caught, or the checks above prove nothing.
mutant="$work/mutant"
mkdir -p "$mutant/hack"
sed 's/|| \[ "\${when\[\$i\]}" = pr \]/|| true/' hack/envtest.sh >"$mutant/hack/envtest.sh"
chmod +x "$mutant/hack/envtest.sh"
if cmp -s hack/envtest.sh "$mutant/hack/envtest.sh"; then
  echo "FAIL mutation: the pr filter in hack/envtest.sh was not found to mutate" | tee -a "$work/results" >&2
elif [ "$(json_list "$(ENVTEST_VERSIONS_TABLE="$PWD/hack/envtest-versions.txt" "$mutant/hack/envtest.sh" matrix pr)")" != "$table_pr" ]; then
  echo "ok   mutation: a matrix that ignores the pr marker is caught" | tee -a "$work/results"
else
  echo "FAIL mutation: a matrix that ignores the pr marker went unnoticed" | tee -a "$work/results" >&2
fi
mutate_ci() { # <name> <sed expression>
  sed "$2" "$ci" >"$work/ci.mutant.yml"
  if cmp -s "$ci" "$work/ci.mutant.yml"; then
    echo "FAIL mutation $1: nothing to mutate in $ci" | tee -a "$work/results" >&2
  elif [ "$(wired "$work/ci.mutant.yml")" != ok ]; then
    echo "ok   mutation: $1 is caught" | tee -a "$work/results"
  else
    echo "FAIL mutation: $1 went unnoticed" | tee -a "$work/results" >&2
  fi
}
mutate_ci "a hard-coded envtest matrix" 's/^\( *\)minor: \${{ fromJSON(needs.kube-matrix.outputs.envtest-minors) }}$/\1minor: ["1.24", "1.25", "1.26", "1.27", "1.28"]/'
mutate_ci "the full matrix on every event" 's/hack\/envtest.sh matrix "\$set"/hack\/envtest.sh matrix all/'
mutate_ci "a missing wait for kube-matrix" "s/needs.kube-matrix.result == 'success' \&\&//"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "envtest_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
