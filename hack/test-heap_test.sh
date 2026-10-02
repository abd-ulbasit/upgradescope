#!/usr/bin/env bash
# Tests for hack/test-heap.sh (make hack-test), offline: its discovery
# finds the heap-bound tests the claims ledger cites (each skips or
# shrinks under -race), in their packages, and nothing that is not a Test
# function. Running them is CI's test-heap job.
set -euo pipefail
cd "$(dirname "$0")/.."

listed=$(hack/test-heap.sh --list)
pass=0 fail=0
for want in \
  "internal/server TestIngestDecodeHeapIsBounded" \
  "internal/server TestGateDecodeHeapIsBounded" \
  "internal/server TestGateClusterContextHeapIsBounded" \
  "internal/server TestReadHeapIsBounded" \
  "internal/server TestStoredSnapshotHeapIsBounded" \
  "internal/server TestFleetReadsLoadNoReport" \
  "internal/server TestUnreadResponsesAreBounded" \
  "internal/server TestUnreadGateResponsesAreBounded" \
  "internal/server TestUnreadFleetResponsesAreBounded" \
  "internal/collect TestCollectHelmPeakHeapIsBoundedByOneRelease"; do
  if grep -qxF "$want" <<<"$listed"; then
    echo "ok   lists $want"
    pass=$((pass + 1))
  else
    echo "FAIL does not list $want" >&2
    fail=$((fail + 1))
  fi
done
if bad=$(grep -vE '^internal/[a-z/]+ Test[A-Za-z0-9_]+$' <<<"$listed"); then
  echo "FAIL lists something other than '<dir> <TestName>': $bad" >&2
  fail=$((fail + 1))
else
  echo "ok   lists only '<dir> <TestName>' lines"
  pass=$((pass + 1))
fi
echo "test-heap_test: $pass passed, $fail failed"
[ "$fail" = 0 ]
