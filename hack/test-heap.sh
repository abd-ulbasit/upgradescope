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
# push, each under 50 ms), not a heap bound.
#
# The server's heap tests take about ten minutes, past go test's default
# timeout, and the Helm manifest test's live-heap reading is inflated by a
# machine busy with every other package's tests, so they also skip unless
# UPGRADESCOPE_HEAP=1, which this sets: a plain `go test ./...` passes
# without them.
#
# Run serially the set took 1209 s on a hosted runner (run 38038631216:
# internal/server 1050 s, internal/collect 152 s, internal/server/store 7 s),
# the longest job of a pull request, so CI runs it as four shards in parallel
# (--shard i/n, the test-heap job's matrix). Each discovered test is assigned to exactly one
# shard by hack/shard.sh, greedy longest-first from the committed duration
# table hack/test-heap-durations.txt ("<package dir> <TestName> <seconds>",
# a test with no entry counts 30 s): a new heap test is picked up and placed
# without editing anything, and the table only sharpens the balance. With no
# --shard every test runs, as `make test-heap` does on a laptop.
#
# --list prints "<package dir> <TestName>" per test and runs nothing; with
# --shard it prints that shard's tests.
set -euo pipefail
export UPGRADESCOPE_HEAP=1
cd "$(dirname "$0")/.."

usage() {
  echo "usage: hack/test-heap.sh [--list] [--shard <i>/<n>]" >&2
  exit 2
}
list=false
shard=""
while [ $# -gt 0 ]; do
  case "$1" in
    --list) list=true; shift ;;
    --shard) [ $# -ge 2 ] || usage; shard=$2; shift 2 ;;
    *) usage ;;
  esac
done

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
if [ -n "$shard" ]; then
  # shard.sh refuses anything but 1 <= i <= n, so a mistyped leg fails here
  # instead of running nothing.
  tests=$(hack/shard.sh "$shard" -d hack/test-heap-durations.txt <<<"$tests")
  if [ -z "$tests" ]; then
    $list || echo "test-heap: OK (shard $shard has no tests: fewer tests than shards)"
    exit 0
  fi
fi
if $list; then
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
echo "test-heap: OK${shard:+ (shard $shard)}"
