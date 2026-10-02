#!/usr/bin/env bash
# The envtest matrix (#135, #69): TestEnvtestMatrix, the real collector and
# engine against a real kube-apiserver and etcd, for Kubernetes minors kind
# has no node image for (1.24 to 1.28, hack/envtest-versions.txt). CI's
# envtest job runs one minor per matrix leg; a laptop runs the same:
#
#   hack/envtest.sh          every minor in the table
#   hack/envtest.sh 1.24     just that one
#
# No Docker, no cluster, no kubeconfig: the apiserver is a local process on
# a loopback port that the test starts and stops itself, so no kube context
# is read or touched. Needs Go and network access on the first run (it
# installs setup-envtest and downloads the bundle).
#
# Supply chain: setup-envtest is pinned to one release (go install is
# checked against the Go checksum database), and it reads its index of
# kube-apiserver/etcd bundles from one pinned controller-tools commit, not
# from a branch, and verifies each download against the sha512 in that
# index. Bump the version, the commit and the table together.
#
# Everything is installed under $ENVTEST_BIN (default bin/envtest, gitignored;
# a relative path is relative to the repository root): CI caches it.
#
# Exit status: 0 passed, 1 failed (or the test did not run), 2 bad request.
#
# Knobs, for hack/envtest_test.sh:
#   ENVTEST_VERSIONS_TABLE  the table (default hack/envtest-versions.txt)
#   ENVTEST_SETUP           a setup-envtest to use instead of installing one
#   ENVTEST_GO_TEST         the command that runs the test (default: go test)
set -euo pipefail
cd "$(dirname "$0")/.."

SETUP_ENVTEST_VERSION=v0.25.2
# kubernetes-sigs/controller-tools, envtest-releases.yaml at this commit.
ENVTEST_INDEX_COMMIT=1031496fc98a4f51010c3bdfdeb57b5d67bea7bd
INDEX="https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/$ENVTEST_INDEX_COMMIT/envtest-releases.yaml"

TABLE=${ENVTEST_VERSIONS_TABLE:-hack/envtest-versions.txt}
ENVTEST_BIN=${ENVTEST_BIN:-bin/envtest}
case "$ENVTEST_BIN" in /*) ;; *) ENVTEST_BIN="$PWD/$ENVTEST_BIN" ;; esac

bad() { echo "envtest: $*" >&2; exit 2; }

minors=() exact=()
while read -r minor version _; do
  case "$minor" in '' | '#'*) continue ;; esac
  [[ "$minor" =~ ^1\.[0-9]+$ ]] || bad "$TABLE: bad minor '$minor'"
  [[ "$version" =~ ^"$minor"\.[0-9]+$ ]] || bad "$TABLE: $minor: '$version' is not an exact $minor.N release"
  minors+=("$minor") exact+=("$version")
done <"$TABLE"
[ ${#minors[@]} -gt 0 ] || bad "$TABLE: no minors"

want=("$@")
[ ${#want[@]} -gt 0 ] || want=("${minors[@]}")
for m in "${want[@]}"; do
  [[ " ${minors[*]} " == *" $m "* ]] || bad "no envtest bundle for '$m' in $TABLE (have: ${minors[*]})"
done

setup=${ENVTEST_SETUP:-}
if [ -z "$setup" ]; then
  setup="$ENVTEST_BIN/setup-envtest-$SETUP_ENVTEST_VERSION/setup-envtest"
  if [ ! -x "$setup" ]; then
    echo "envtest: installing setup-envtest $SETUP_ENVTEST_VERSION" >&2
    GOBIN="$(dirname "$setup")" go install "sigs.k8s.io/controller-runtime/tools/setup-envtest@$SETUP_ENVTEST_VERSION"
  fi
fi

run_test() {
  if [ -n "${ENVTEST_GO_TEST:-}" ]; then
    $ENVTEST_GO_TEST
  else
    go test ./internal/collect -run '^TestEnvtestMatrix$' -count=1 -v
  fi
}

log=$(mktemp)
trap 'rm -f "$log"' EXIT
for m in "${want[@]}"; do
  for i in "${!minors[@]}"; do
    [ "${minors[$i]}" = "$m" ] && version=${exact[$i]}
  done
  echo "envtest: Kubernetes $m (kube-apiserver and etcd $version)" >&2
  assets=$("$setup" use "$version" --index "$INDEX" --bin-dir "$ENVTEST_BIN/assets" -p path)
  KUBEBUILDER_ASSETS=$assets UPGRADESCOPE_ENVTEST=1 UPGRADESCOPE_ENVTEST_MINOR=$m run_test 2>&1 | tee "$log"
  # go test passes when -run matches nothing, and a skipped test passes
  # too: only the test's own PASS line counts.
  grep -q '^--- PASS: TestEnvtestMatrix ' "$log" ||
    { echo "envtest: TestEnvtestMatrix did not run and pass on Kubernetes $m" >&2; exit 1; }
done
echo "envtest: ok (${want[*]})" >&2
