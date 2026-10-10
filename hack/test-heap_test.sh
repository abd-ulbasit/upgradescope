#!/usr/bin/env bash
# Tests for hack/test-heap.sh (make hack-test), offline: its discovery
# finds the heap-bound tests the claims ledger cites (each skips or
# shrinks under -race), in their packages, and nothing that is not a Test
# function; its --shard i/n runs every discovered test in exactly one
# shard (also a test the duration table has never heard of), and CI's
# matrix names every shard, so none can go missing. Running the tests is
# CI's test-heap job.
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

listed=$(hack/test-heap.sh --list)
pass=0 fail=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1" >&2; fail=$((fail + 1)); }

for want in \
  "internal/server TestIngestDecodeHeapIsBounded" \
  "internal/server TestGateDecodeHeapIsBounded" \
  "internal/server TestGateClusterContextHeapIsBounded" \
  "internal/server TestGateAnswerHeapIsBounded" \
  "internal/server TestReadHeapIsBounded" \
  "internal/server TestStoredSnapshotHeapIsBounded" \
  "internal/server TestFleetReadsLoadNoReport" \
  "internal/server TestUnreadResponsesAreBounded" \
  "internal/server TestUnreadGateResponsesAreBounded" \
  "internal/server TestUnreadFleetResponsesAreBounded" \
  "internal/server TestFleetDefaultColumnsAreMeasured" \
  "internal/server TestFleetReadsOfTheWidestGapsAreBounded" \
  "internal/collect TestCollectHelmPeakHeapIsBoundedByOneRelease" \
  "internal/collect TestCollectHelmGzipBombIsBounded" \
  "internal/collect TestCollectHelmManifestParsingIsBounded" \
  "internal/collect TestParseOnce_NumberDenseStreamIsNotSlower"; do
  if grep -qxF "$want" <<<"$listed"; then
    ok "lists $want"
  else
    bad "does not list $want"
  fi
done
if bad=$(grep -vE '^internal/[a-z/]+ Test[A-Za-z0-9_]+$' <<<"$listed"); then
  bad "lists something other than '<dir> <TestName>': $bad"
else
  ok "lists only '<dir> <TestName>' lines"
fi
# The server's heap tests and the Helm manifest test skip unless
# UPGRADESCOPE_HEAP=1 (heapRun): the script must set it, or it would fail
# every one of them as skipped.
if grep -qx 'export UPGRADESCOPE_HEAP=1' hack/test-heap.sh; then
  ok "sets UPGRADESCOPE_HEAP=1"
else
  bad "does not set UPGRADESCOPE_HEAP=1"
fi

# ---- sharding -------------------------------------------------------------

# The union of the n shards is --list, and no test is in two, for every n
# from 1 to 6 (the duration table is read, so this is the real set).
sorted_list=$(sort <<<"$listed")
for n in 1 2 3 4 5 6; do
  all=$(for i in $(seq 1 "$n"); do hack/test-heap.sh --list --shard "$i/$n" 2>/dev/null; done | sort)
  if [ -n "$(uniq -d <<<"$all")" ]; then
    bad "$n shards: a test is in two shards: $(uniq -d <<<"$all" | head -2 | tr '\n' ' ')"
  elif [ "$all" != "$sorted_list" ]; then
    bad "$n shards: the union is not --list"
  else
    ok "$n shards: every discovered test is in exactly one shard ($(wc -l <<<"$all" | tr -d ' ') tests)"
  fi
done
for i in 1 2 3 4; do
  if [ -n "$(hack/test-heap.sh --list --shard "$i/4" 2>/dev/null)" ]; then ok "shard $i/4 is not empty"; else bad "shard $i/4 is empty"; fi
done

# The duration table: "<dir> <TestName> <seconds>" lines, no test twice.
table=hack/test-heap-durations.txt
if lines=$(grep -vE '^(#.*|[[:space:]]*|internal/[a-z/]+ Test[A-Za-z0-9_]+ [0-9]+)$' "$table"); then
  bad "$table has a line that is not '<dir> <TestName> <seconds>': $lines"
else
  ok "$table holds only '<dir> <TestName> <seconds>' lines"
fi
if dup=$(grep -vE '^(#|[[:space:]]*$)' "$table" | awk '{print $1, $2}' | sort | uniq -d | grep .); then
  bad "$table lists a test twice: $dup"
else
  ok "$table lists no test twice"
fi

# A mistyped shard fails instead of running nothing.
for spec in 0/4 5/4 4 a/b -1/4 1/0; do
  if hack/test-heap.sh --list --shard "$spec" >/dev/null 2>&1; then bad "--shard $spec was accepted"; else ok "--shard $spec is refused"; fi
done
if hack/test-heap.sh --shard >/dev/null 2>&1; then bad "--shard with no value was accepted"; else ok "--shard with no value is refused"; fi
if hack/test-heap.sh --nope >/dev/null 2>&1; then bad "an unknown flag was accepted"; else ok "an unknown flag is refused"; fi

# A scratch repository with the scripts, a table that knows only one of its
# tests, and tests the table has never heard of ("a new test appears"): each
# runs in exactly one shard, listed and run.
fx=$work/repo
mkdir -p "$fx/hack" "$fx/internal/alpha" "$fx/internal/beta" "$fx/internal/gamma/sub" "$fx/bin"
cp hack/test-heap.sh hack/shard.sh "$fx/hack/"
printf 'internal/alpha TestAlphaHeap 200\n' >"$fx/hack/test-heap-durations.txt"
mktest() { # <dir> <file> <TestName>...: a test file whose functions read raceEnabled
  local d=$1 f=$2
  shift 2
  {
    echo "package x"
    for t in "$@"; do printf 'func %s(t *testing.T) {\n\tif raceEnabled {\n\t\tt.Skip()\n\t}\n}\n' "$t"; done
  } >"$fx/$d/$f"
}
mktest internal/alpha a_test.go TestAlphaHeap TestAlphaOther
mktest internal/beta b_test.go TestBetaHeap
mktest internal/gamma/sub c_test.go TestBrandNewHeap
printf 'package x\nfunc TestNotAHeapTest(t *testing.T) {\n}\n' >"$fx/internal/beta/plain_test.go"
(cd "$fx" && git init -q && git add -A && git -c user.name=t -c user.email=t@t commit -qm fixture)
fx_all=$(cd "$fx" && hack/test-heap.sh --list | sort)
want=$'internal/alpha TestAlphaHeap\ninternal/alpha TestAlphaOther\ninternal/beta TestBetaHeap\ninternal/gamma/sub TestBrandNewHeap'
if [ "$fx_all" = "$want" ]; then ok "fixture: discovery finds the four tests that read raceEnabled"; else bad "fixture discovery: $fx_all"; fi
for n in 1 2 3 4 5; do
  got=$(for i in $(seq 1 "$n"); do (cd "$fx" && hack/test-heap.sh --list --shard "$i/$n" 2>/dev/null); done | sort)
  if [ "$got" = "$want" ]; then ok "fixture, $n shards: each test (the new ones too) is in exactly one"; else bad "fixture, $n shards: got $(echo $got)"; fi
done

# Run mode, with a stub `go` that records its arguments: across the shards
# every test is run once, the heap switch is on, and without --shard all of
# them run.
cat >"$fx/bin/go" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$STUB_LOG"
[ -z "${STUB_SKIP:-}" ] || echo "--- SKIP: TestAlphaHeap (0.00s)"
echo "stub: UPGRADESCOPE_HEAP=${UPGRADESCOPE_HEAP:-}"
exit "${STUB_RC:-0}"
STUB
chmod +x "$fx/bin/go"
run_names() { # <log>: "<dir> <TestName>" for each test go was asked to run
  awk '{
    d = $2; sub(/^\.\//, "", d)
    for (i = 1; i <= NF; i++) if ($i == "-run") {
      r = $(i + 1); gsub(/[\^$()]/, "", r)
      n = split(r, t, "|")
      for (k = 1; k <= n; k++) print d, t[k]
    }
  }' "$1" | sort
}
: >"$work/log-all"
(cd "$fx" && PATH="$fx/bin:$PATH" STUB_LOG="$work/log-all" hack/test-heap.sh >"$work/out-all" 2>&1)
if [ "$(run_names "$work/log-all")" = "$want" ]; then ok "fixture: with no --shard every test runs"; else bad "fixture, no shard: ran $(run_names "$work/log-all" | tr '\n' ' ')"; fi
if grep -q 'UPGRADESCOPE_HEAP=1' "$work/out-all"; then ok "fixture: the heap switch is on in go test"; else bad "the heap switch is not set when go runs"; fi
if grep -qE -- ' -count=1 ' "$work/log-all" && ! grep -q -- '-race' "$work/log-all"; then ok "fixture: -count=1 and no -race"; else bad "go test flags: $(cat "$work/log-all")"; fi
: >"$work/log-sh"
for i in 1 2 3; do
  (cd "$fx" && PATH="$fx/bin:$PATH" STUB_LOG="$work/log-sh" hack/test-heap.sh --shard "$i/3" >"$work/out-$i" 2>&1) || bad "fixture: shard $i/3 failed: $(cat "$work/out-$i")"
done
if [ "$(run_names "$work/log-sh")" = "$want" ]; then ok "fixture: shards 1/3, 2/3 and 3/3 together run each test once"; else bad "fixture, 3 shards: ran $(run_names "$work/log-sh" | tr '\n' ' ')"; fi
# A heap test that skipped itself is a proof that did not run: fail on it,
# in a shard too. A failing go test fails the shard.
if (cd "$fx" && PATH="$fx/bin:$PATH" STUB_LOG=/dev/null STUB_SKIP=1 hack/test-heap.sh --shard 1/2 >/dev/null 2>&1); then bad "a skipped heap test passed the shard"; else ok "a skipped heap test fails the shard"; fi
if (cd "$fx" && PATH="$fx/bin:$PATH" STUB_LOG=/dev/null STUB_RC=1 hack/test-heap.sh --shard 1/2 >/dev/null 2>&1); then bad "a failing go test passed the shard"; else ok "a failing go test fails the shard"; fi
# More shards than tests: the empty ones pass (nothing was left to prove).
if (cd "$fx" && PATH="$fx/bin:$PATH" STUB_LOG=/dev/null hack/test-heap.sh --shard 9/9 >/dev/null 2>&1); then ok "a shard with no tests passes"; else bad "a shard with no tests failed"; fi

# ---- the wiring -----------------------------------------------------------
ci=.github/workflows/ci.yml
block=$(awk '$0 == "  test-heap:" { c = 1; next } c && /^  [a-z0-9_-]+:[ ]*$/ { c = 0 } c' "$ci")
[ -n "$block" ] || { echo "FAIL $ci has no test-heap job" >&2; exit 1; }
# matrix: shard: ['1/4', '2/4', ...]: every i of n, once, n the same in all.
shards=$(sed -n 's/^ *shard: *\[\(.*\)\].*/\1/p' <<<"$block" | tr ',' '\n' | tr -d " '\"" | sed '/^$/d')
n=$(sed -n '$=' <<<"$shards")
n=${n:-0}
want_shards=$(seq 1 "$n" | sed "s#\$#/$n#")
if [ "$n" -lt 3 ]; then
  bad "$ci's test-heap job has no matrix of at least 3 shards (shard: ['1/n', ...])"
elif [ "$(sort <<<"$shards")" != "$(sort <<<"$want_shards")" ]; then
  bad "$ci's test-heap matrix is not every shard 1/$n..$n/$n exactly once: $(echo $shards)"
else
  ok "$ci's test-heap matrix runs every shard of $n, once"
fi
if grep -q 'fail-fast: false' <<<"$block"; then ok "test-heap's matrix does not stop at the first failing shard"; else bad "test-heap's matrix is fail-fast"; fi
if grep -qF 'make test-heap TEST_HEAP_SHARD="$SHARD"' <<<"$block" && grep -qF 'SHARD: ${{ matrix.shard }}' <<<"$block"; then
  ok "the job passes its matrix shard to make test-heap"
else
  bad "test-heap's run does not pass matrix.shard (env SHARD) to make test-heap TEST_HEAP_SHARD"
fi
if grep -qF -- '--shard $(TEST_HEAP_SHARD)' Makefile; then ok "make test-heap takes TEST_HEAP_SHARD"; else bad "the Makefile's test-heap does not pass TEST_HEAP_SHARD to --shard"; fi

echo "test-heap_test: $pass passed, $fail failed"
[ "$fail" = 0 ]
