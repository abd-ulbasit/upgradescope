#!/usr/bin/env bash
# The heap-bound tests, without the race detector (make test-heap; CI's
# test-heap job). The race detector instruments every allocation, so a test
# whose proof is a heap figure skips itself, or shrinks, when raceEnabled
# is set, and `make test` (go test -race) never runs it in full. These
# tests are the proofs docs/claims.md and docs/operations.md cite for the
# server's memory bounds (SE-05b, SE-05c, PF-06) and the Helm collector's
# (HE-04), so this runs them, every one, -count=1.
#
# The set is discovered, not listed: every Test function in a tracked
# _test.go file of the main module whose body reads raceEnabled. A new
# heap test that skips under -race is picked up without editing this
# script, and one cannot be cited yet silently never run.
#
# Not every test in the set is a heap proof: the criterion is reading
# raceEnabled, and a timing proof that skips under -race because the
# detector's slowdown invalidates its bounds reads it too. So
# TestPruneWithNothingToDeleteIsCheap (internal/server/store, #297) is in the
# set and is a timing proof (a prune that deletes nothing, and a concurrent
# push, each under 50 ms), not a heap bound. So is
# TestParseOnce_NumberDenseStreamIsNotSlower (internal/collect, #285), a CPU
# ratio (a stream parsed once costs under 0.8 of the same parsed twice) that
# could flake beside every other package's tests, so it also skips unless
# UPGRADESCOPE_HEAP=1.
#
# The server's heap tests take about ten minutes, past go test's default
# timeout, and the Helm manifest test's live-heap reading is inflated by a
# machine busy with every other package's tests, so they also skip unless
# UPGRADESCOPE_HEAP=1, which this sets: a plain `go test ./...` passes
# without them.
#
# --list prints "<package dir> <TestName>" per test and runs nothing.
set -euo pipefail
export UPGRADESCOPE_HEAP=1
cd "$(dirname "$0")/.."

# "<dir> <TestName>" for each test function that mentions raceEnabled.
discover() {
  git ls-files -z -- '*_test.go' ':!:tools/**' | xargs -0 awk '
    FNR == 1 { fn = "" }
    match($0, /^func Test[A-Za-z0-9_]+\(/) { fn = substr($0, 6, RLENGTH - 6); next }
    /^}/ { fn = ""; next }
    fn != "" && /raceEnabled/ {
      dir = FILENAME; sub(/\/[^\/]*$/, "", dir)
      print dir, fn; fn = ""
    }' | sort -u
}

tests=$(discover)
if [ -z "$tests" ]; then
  echo "test-heap: no test reads raceEnabled; the discovery is broken" >&2
  exit 1
fi
if [ "${1:-}" = --list ]; then
  echo "$tests"
  exit 0
fi

log=$(mktemp)
trap 'rm -f "$log"' EXIT
for dir in $(cut -d' ' -f1 <<<"$tests" | sort -u); do
  names=$(awk -v d="$dir" '$1 == d { print $2 }' <<<"$tests" | paste -sd'|' -)
  echo "== go test ./$dir -count=1 -run '^($names)\$' (no -race)"
  status=0
  go test "./$dir" -count=1 -timeout 30m -v -run "^($names)\$" >"$log" 2>&1 || status=$?
  # -v for the measured figures the tests log; the rest of it trimmed.
  grep -vE '^=== (RUN|PAUSE|CONT)' "$log" || true
  [ "$status" = 0 ] || { echo "test-heap: go test ./$dir failed" >&2; exit "$status"; }
  # A skip here is a heap proof that did not run: fail on it.
  if grep -qE '^ *--- SKIP' "$log"; then
    echo "test-heap: a heap-bound test skipped itself in ./$dir" >&2
    exit 1
  fi
done
echo "test-heap: OK"
