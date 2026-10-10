#!/usr/bin/env bash
# Tests for hack/shard.sh (make hack-test), offline: the split CI's test-heap
# and test matrices share must give every item to exactly one shard, give the
# same answer on every run, balance by the durations table (longest first),
# and place an item the table does not know instead of dropping it.
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pass=0 fail=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1" >&2; fail=$((fail + 1)); }
check() { # <name> <want> <got>
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: want $(echo $2) got $(echo $3)"; fi
}

cat >"$work/durations" <<'EOF'
# a comment, then a blank line

big one 100
b 90
c 50
d 40
   e   30.5
EOF

# Greedy longest-first on 100, 90, 50, 40, 30.5, and an unknown item at the
# 30 s default: 2 shards take big one(100) | b(90), c -> shard 2 (90 < 100)
# = 140, d -> shard 1 (100 < 140) = 140, e -> shard 1 (tie: lowest index)
# = 170.5, new(30) -> shard 2 = 170.
printf 'big one\nb\nc\nd\ne\nnew\n' >"$work/items"
check "2 shards: shard 1" "big one d e" "$(hack/shard.sh 1/2 -d "$work/durations" <"$work/items" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"
check "2 shards: shard 2" "b c new" "$(hack/shard.sh 2/2 -d "$work/durations" <"$work/items" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"

# One shard is everything.
check "1 shard is all of it" "b big one c d e new" "$(hack/shard.sh 1/1 -d "$work/durations" <"$work/items" 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"

# Deterministic: the same input in another order, and a repeat, give the
# same shards.
for i in 1 2 3; do
  a=$(hack/shard.sh "$i/3" -d "$work/durations" <"$work/items" 2>/dev/null)
  b=$(sort -r "$work/items" | hack/shard.sh "$i/3" -d "$work/durations" 2>/dev/null)
  c=$(hack/shard.sh "$i/3" -d "$work/durations" <"$work/items" 2>/dev/null)
  [ "$a" = "$b" ] && [ "$a" = "$c" ] && ok "shard $i/3 is the same on every run and for any input order" ||
    bad "shard $i/3 differs between runs"
done

# Every item in exactly one shard, for 1..7 shards, with a table that knows
# some of them and a list holding duplicates, spaces and new items.
{
  for k in $(seq 1 40); do echo "internal/pkg$k TestHeap$k"; done
  echo "internal/pkg1 TestHeap1" # a duplicate line is one item
  echo "  "                      # a blank line is no item
} >"$work/many"
for k in 1 3 7 11 17; do echo "internal/pkg$k TestHeap$k $((k * 13))"; done >"$work/many-durations"
sort -u "$work/many" | sed '/^ *$/d' >"$work/many-want"
for n in 1 2 3 4 5 7 40 41 60; do
  : >"$work/union"
  for i in $(seq 1 "$n"); do hack/shard.sh "$i/$n" -d "$work/many-durations" <"$work/many" 2>/dev/null >>"$work/union"; done
  if [ "$(sort "$work/union" | uniq -d | wc -l | tr -d ' ')" != 0 ]; then
    bad "$n shards: an item is in two shards: $(sort "$work/union" | uniq -d | head -3 | tr '\n' ' ')"
  elif ! diff -q <(sort "$work/union") "$work/many-want" >/dev/null; then
    bad "$n shards: the union is not the input"
  else
    ok "$n shards: every item in exactly one shard ($(wc -l <"$work/many-want" | tr -d ' ') items)"
  fi
done

# Balance: the busiest shard is within one item of the mean (the bound for
# greedy longest-first with unit-ish items), with the real table's numbers.
printf 'a 277\nb 225\nc 206\nd 120\ne 109\nf 62\ng 60\nh 35\ni 30\nj 19\nk 10\nl 9\nm 8\nn 2\n' >"$work/real"
cut -d' ' -f1 "$work/real" >"$work/real-items"
total=$(awk '{s += $2} END {print s}' "$work/real")
for n in 3 4; do
  max=$(for i in $(seq 1 "$n"); do
    hack/shard.sh "$i/$n" -d "$work/real" <"$work/real-items" 2>/dev/null | awk -v t="$work/real" 'BEGIN { while ((getline l < t) > 0) { split(l, f, " "); c[f[1]] = f[2] } } { s += c[$1] } END { print s + 0 }'
  done | sort -n | tail -1)
  # LPT is within 4/3 of optimal; the optimum is at least max(mean, biggest).
  bound=$(awk -v t="$total" -v n="$n" 'BEGIN { m = t / n; if (m < 277) m = 277; printf "%d", m * 4 / 3 }')
  [ "$max" -le "$bound" ] && ok "$n shards: busiest ${max}s is within 4/3 of the optimum (<= ${bound}s)" ||
    bad "$n shards: busiest ${max}s exceeds ${bound}s"
done

# An item without an entry is placed (30 s) and named on stderr; a known one
# is not.
err=$(printf 'big one\nnew-test\n' | hack/shard.sh 1/1 -d "$work/durations" 2>&1 >/dev/null)
grep -qF 'new-test' <<<"$err" && ! grep -qF 'big one' <<<"$err" && ok "an item with no duration is named on stderr" ||
  bad "stderr does not name exactly the unknown item: $err"
err=$(printf 'x\n' | hack/shard.sh 1/1 -c 7 2>&1 >/dev/null)
grep -qF '7s assumed' <<<"$err" && ok "-c sets the default cost" || bad "-c is ignored: $err"

# No input: empty shards, not an error.
check "no items, no output" "" "$(: | hack/shard.sh 2/3 2>/dev/null)"

# Refusals.
refuses() { # <name> <args...>
  local name=$1
  shift
  if printf 'a\n' | hack/shard.sh "$@" >/dev/null 2>&1; then bad "$name was accepted"; else ok "$name is refused"; fi
}
refuses "no argument"
refuses "0/3" 0/3
refuses "4/3" 4/3
refuses "1/0" 1/0
refuses "a spec that is not i/n" one/two
refuses "a negative index" -1/3
refuses "a missing durations file" 1/2 -d "$work/nope"
refuses "a non-numeric default cost" 1/2 -c fast
refuses "an unknown flag" 1/2 -x
printf 'no-seconds-here\n' >"$work/malformed"
refuses "a durations line with no seconds" 1/2 -d "$work/malformed"
printf 'item 12s\n' >"$work/malformed2"
refuses "a durations line with a unit" 1/2 -d "$work/malformed2"

echo "shard_test: $pass passed, $fail failed"
[ "$fail" = 0 ]
