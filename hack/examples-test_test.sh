#!/usr/bin/env bash
# Tests for hack/examples-test.sh. Offline: the three tools are stubs that
# record their arguments, so the cases prove what the script asks each one to
# check and that one failing check neither hides nor skips the others.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# stub <name>: records "<name> <args>" and exits with $STUB_<NAME>_EXIT.
for t in kyverno gator validator; do
  cat >"$work/$t" <<EOF
#!/usr/bin/env bash
echo "$t \$*" >>"$work/calls"
exit \${STUB_${t}_EXIT:-0}
EOF
  chmod +x "$work/$t"
done

: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() { echo "FAIL $1" | tee -a "$work/results" >&2; }
# run [env...]: the script with the stubs, output in $work/out, status in $got.
run() {
  : >"$work/calls"
  got=0
  env EXAMPLES_KYVERNO="$work/kyverno" EXAMPLES_GATOR="$work/gator" \
    EXAMPLES_RENOVATE_VALIDATOR="$work/validator" "$@" hack/examples-test.sh >"$work/out" 2>&1 || got=$?
}
called() { grep -qxF "$1" "$work/calls"; }

run
if [ "$got" = 0 ]; then ok "all three checks passing passes"; else fail "passing run exited $got"; fi
called "kyverno test examples/policies/kyverno/tests --require-tests" && ok "kyverno runs the policy tests, and fails when there are none" ||
  fail "kyverno was not asked to test the policy folder: $(cat "$work/calls")"
called "gator verify examples/policies/gatekeeper/tests/suite.yaml" && ok "gator verifies the suite" ||
  fail "gator was not asked to verify the suite: $(cat "$work/calls")"
called "validator --strict --no-global examples/renovate/upgradescope.json" && ok "the preset is validated strictly, as a repo config" ||
  fail "the validator was not asked to check the preset strictly: $(cat "$work/calls")"

# The files the commands name exist.
for f in examples/policies/kyverno/tests examples/policies/gatekeeper/tests/suite.yaml examples/renovate/upgradescope.json; do
  [ -e "$f" ] && ok "$f exists" || fail "$f does not exist"
done

for which in kyverno gator validator; do
  run "STUB_${which}_EXIT=1"
  if [ "$got" = 1 ] && [ "$(wc -l <"$work/calls" | tr -d ' ')" = 3 ]; then
    ok "a failing $which fails the run after the other two ran"
  else
    fail "a failing $which: exit $got, calls: $(cat "$work/calls")"
  fi
done

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "examples-test_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
