#!/usr/bin/env bash
# Checks the example policies and the Renovate preset without a cluster
# (make examples-test; CI's examples job):
#
#   - kyverno test examples/policies/kyverno/tests: the ClusterPolicy against
#     a ready, a not-ready, a stale, an unassessed and a missing
#     ClusterReadiness (the Kyverno CLI answers the policy's apiCall with the
#     fixture, so no apiserver is involved);
#   - gator verify examples/policies/gatekeeper/tests/suite.yaml: the
#     ConstraintTemplate and Constraint against the same cases, with the
#     ClusterReadiness as replicated data;
#   - renovate-config-validator --strict --no-global on
#     examples/renovate/upgradescope.json, from the Renovate release pinned
#     in hack/renovate (npm ci, install scripts off: the validator then falls
#     back from RE2 to RegExp and says so).
#
# kyverno and gator are the pinned, sha256-verified downloads of
# hack/install-tool.sh. Every check runs even when an earlier one failed, so
# one run reports all of them. The offline half (fixtures decode as the
# agent's status, the policies name the right object, the preset matches the
# registry) is `go test ./examples`.
#
# Needs network on the first run (the tools, the npm registry), and Node 24
# for Renovate (CI's examples job sets it up).
#
# Knobs, for hack/examples-test_test.sh: EXAMPLES_KYVERNO, EXAMPLES_GATOR and
# EXAMPLES_RENOVATE_VALIDATOR replace the three commands (nothing is
# downloaded or installed for one that is set).
set -euo pipefail
cd "$(dirname "$0")/.."

failed=()
# check <name> <command...>: runs it, records a failure, goes on.
check() {
  local name=$1
  shift
  echo "== $name"
  if "$@"; then
    echo "ok   $name"
  else
    echo "FAIL $name" >&2
    failed+=("$name")
  fi
}

kyverno=${EXAMPLES_KYVERNO:-}
[ -n "$kyverno" ] || kyverno=$(hack/install-tool.sh kyverno)
gator=${EXAMPLES_GATOR:-}
[ -n "$gator" ] || gator=$(hack/install-tool.sh gator)
validator=${EXAMPLES_RENOVATE_VALIDATOR:-}
if [ -z "$validator" ]; then
  (cd hack/renovate && npm ci --ignore-scripts --no-fund --no-audit)
  validator="$PWD/hack/renovate/node_modules/.bin/renovate-config-validator"
fi

check "kyverno test" "$kyverno" test examples/policies/kyverno/tests --require-tests
check "gator verify" "$gator" verify examples/policies/gatekeeper/tests/suite.yaml
check "renovate-config-validator" "$validator" --strict --no-global examples/renovate/upgradescope.json

if [ ${#failed[@]} -gt 0 ]; then
  echo "examples-test: failed: ${failed[*]}" >&2
  exit 1
fi
echo "examples-test: OK"
