#!/usr/bin/env bash
# The no-helm leg of the unit gate (make hack-test): the Go packages that
# shell out to helm skip, not fail, on a machine without it, which keeps
# CONTRIBUTING.md's "unit tests ... need only Go" true (#376). It runs
#   go test ./deploy/chart/ and, in its own module, ./tools/promrule-test/
# with PATH holding only a symlink to go plus /usr/bin:/bin and
# UPGRADESCOPE_CHART_TEST unset, and expects exit 0 with the read-closure
# test skipped in every mode. A mutant (the render closure back outside
# t.Run, bound to the parent test, applied through go's -overlay so no file
# changes) must fail the same run, so the check is not vacuous.
#
# The list of packages is derived: any Go test file that looks helm up or
# runs it must be in one of those two directories, or this fails and names
# the new one. CI's test job has helm preinstalled and cannot see the
# difference; this script is the only thing that can.
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C
root=$PWD
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pass=0 fail=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1" >&2; fail=$((fail + 1)); }

gobin=$(command -v go) || { echo "FAIL go not found" >&2; exit 1; }
mkdir "$work/bin"
ln -s "$gobin" "$work/bin/go"
nohelm="$work/bin:/usr/bin:/bin"
if PATH=$nohelm command -v helm >/dev/null 2>&1; then
  echo "FAIL helm is in /usr/bin or /bin; this check cannot hide it" >&2
  exit 1
fi

# ---- the packages that need helm are exactly the two this leg runs -------
callers=$(git grep -lE 'LookPath\("helm"\)|Command\("helm"|Command\(helm' -- '*.go' | xargs -n1 dirname | sort -u)
want=$'deploy/chart\ntools/promrule-test'
if [ "$callers" = "$want" ]; then
  ok "only deploy/chart and tools/promrule-test run helm from Go tests"
else
  bad "Go tests that run helm are in: $(echo $callers); this leg covers only $(echo $want): add the new one to hack/test-nohelm_test.sh"
fi

# run_chart <extra go test args...>: the chart package with helm hidden.
run_chart() {
  (
    unset UPGRADESCOPE_CHART_TEST
    PATH=$nohelm go test -count=1 -v "$@" ./deploy/chart/
  )
}

# ---- the real tree: both packages pass, the read-closure test skips ------
if run_chart >"$work/chart.out" 2>&1; then
  ok "go test ./deploy/chart/ without helm exits 0"
else
  bad "go test ./deploy/chart/ without helm failed:"
  tail -20 "$work/chart.out" >&2
fi
for mode in 'closed_(no_read_token)' 'open_(server.allowAnonymousRead)' 'open_(server.ingress.allowAnonymousRead)' 'read_token'; do
  if grep -qF -- "--- SKIP: TestServerExtraArgReadFlagsMustNotContradictTheChart/$mode" "$work/chart.out"; then
    ok "read-closure test, mode $mode: skipped"
  else
    bad "read-closure test, mode $mode: no SKIP line"
  fi
done
grep -q -- '^--- FAIL' "$work/chart.out" && bad "a chart test failed without helm" || ok "no chart test fails without helm"

if (cd tools/promrule-test && unset UPGRADESCOPE_CHART_TEST && PATH=$nohelm go test -count=1 -v ./... >"$work/promrule.out" 2>&1); then
  grep -q -- '--- SKIP' "$work/promrule.out" && ok "go test ./tools/promrule-test/ without helm exits 0 with a skip" || bad "promrule-test did not skip without helm"
else
  bad "go test ./tools/promrule-test/ without helm failed:"
  tail -20 "$work/promrule.out" >&2
fi

# ---- the mutant: render bound to the parent t must fail the same run ------
f=deploy/chart/read_closure_test.go
awk -v out="$work/mutant.go" '
  { line[NR] = $0 }
  END {
    for (i = 1; i <= NR; i++) {
      if (!r && line[i] ~ /t\.Run\(m\.name, func\(t \*testing\.T\) \{/) r = i
      if (!s && line[i] ~ /render := func\(args \.\.\.string\) string \{/) s = i
    }
    if (!r || !s || s < r) exit 1
    for (e = s; e <= NR && line[e] != "\t\t\t}"; e++) ;
    for (i = 1; i < r; i++) print line[i] > out
    for (i = s; i <= e; i++) { l = line[i]; sub(/^\t/, "", l); print l > out }
    for (i = r; i < s; i++) print line[i] > out
    for (i = e + 1; i <= NR; i++) print line[i] > out
  }' "$f" || { bad "could not build the mutant from $f (did its render closure move?)"; }
if [ -s "$work/mutant.go" ]; then
  printf '{"Replace":{"%s/%s":"%s"}}\n' "$root" "$f" "$work/mutant.go" >"$work/overlay.json"
  if run_chart -overlay "$work/overlay.json" >"$work/mutant.out" 2>&1; then
    bad "mutant (render bound to the parent t) passed without helm: the check cannot catch the bug"
  else
    grep -q 'TestServerExtraArgReadFlagsMustNotContradictTheChart' "$work/mutant.out" &&
      ok "mutant (render bound to the parent t) fails without helm" ||
      bad "mutant failed, but not in the read-closure test"
  fi
fi

echo "test-nohelm_test: $pass passed, $fail failed"
[ "$fail" = 0 ]
