#!/usr/bin/env bash
# Splits a list of work items into balanced shards, deterministically (the
# one assignment test-heap.sh and test.sh share, so CI's matrix legs agree on
# who runs what). Items are lines on stdin; the shard's own items go to
# stdout, sorted.
#
#   hack/shard.sh <i>/<n> [-d <durations file>] [-c <default seconds>] < items
#
# Greedy longest-first: items are taken from the costliest down (ties by
# name, byte order) and each goes to the shard with the least load so far
# (ties to the lowest index). The cost of an item is its entry in the
# durations file, or the default (30 s) when it has none: a new item is
# placed, never dropped, and never lands in two shards. A duplicate line is
# one item.
#
# The durations file holds "<item> <seconds>" lines, the seconds being the
# last field and the item everything before it (an item may hold spaces);
# blank lines and #-comments are ignored. A malformed line fails the run.
#
# Items without an entry are named on stderr, with the shard's expected load,
# so a stale table shows in the CI log.
set -euo pipefail
export LC_ALL=C

usage() {
  echo "usage: shard.sh <i>/<n> [-d <durations file>] [-c <default seconds>] < items" >&2
  exit 2
}

[ $# -ge 1 ] || usage
spec=$1
shift
durations=""
default=30
while [ $# -gt 0 ]; do
  case "$1" in
    -d) [ $# -ge 2 ] || usage; durations=$2; shift 2 ;;
    -c) [ $# -ge 2 ] || usage; default=$2; shift 2 ;;
    *) usage ;;
  esac
done
[[ "$spec" =~ ^([1-9][0-9]*)/([1-9][0-9]*)$ ]] || { echo "shard: '$spec' is not <i>/<n> (1 <= i <= n)" >&2; exit 2; }
shard=${BASH_REMATCH[1]}
count=${BASH_REMATCH[2]}
[ "$shard" -le "$count" ] || { echo "shard: '$spec' has i > n" >&2; exit 2; }
[[ "$default" =~ ^[0-9]+(\.[0-9]+)?$ ]] || { echo "shard: default cost '$default' is not a number" >&2; exit 2; }
if [ -n "$durations" ] && [ ! -f "$durations" ]; then
  echo "shard: no durations file $durations" >&2
  exit 2
fi

items=$(mktemp)
trap 'rm -f "$items"' EXIT
sed '/^[[:space:]]*$/d' | sort -u >"$items"

# One awk pass costs the items; sort puts them costliest first; the second
# pass deals them out. The durations file is read first (NR == FNR is false
# for it only when it is the first file), so it is passed first.
awk -v dflt="$default" -v dfile="${durations:-/dev/null}" '
  BEGIN {
    while ((getline line < dfile) > 0) {
      if (line ~ /^[[:space:]]*(#|$)/) continue
      if (!match(line, /[[:space:]]+[0-9]+(\.[0-9]+)?[[:space:]]*$/)) {
        print "shard: malformed durations line (want \"<item> <seconds>\"): " line > "/dev/stderr"
        bad = 1; exit 2
      }
      secs = substr(line, RSTART); gsub(/[[:space:]]/, "", secs)
      key = substr(line, 1, RSTART - 1); sub(/^[[:space:]]+/, "", key)
      cost[key] = secs + 0
    }
  }
  { printf "%s\t%s\t%s\n", ($0 in cost) ? cost[$0] : dflt + 0, ($0 in cost) ? "k" : "u", $0 }
  END { if (bad) exit 2 }
' "$items" |
  sort -t "$(printf '\t')" -k1,1gr -k3,3 |
  awk -F '\t' -v dflt="$default" -v shard="$shard" -v count="$count" '
    BEGIN { for (s = 1; s <= count; s++) load[s] = 0 }
    {
      best = 1
      for (s = 2; s <= count; s++) if (load[s] < load[best]) best = s
      load[best] += $1
      if (best == shard) { print $3; mine++; if ($2 == "u") unknown[++nu] = $3 }
    }
    END {
      printf "shard %d/%d: %d items, expected load %gs (the busiest shard: %gs)\n", shard, count, mine + 0, load[shard], maxload(load, count) > "/dev/stderr"
      if (nu > 0) {
        printf "shard: %d items of this shard have no duration, %gs assumed (add them to the durations file):\n", nu, dflt > "/dev/stderr"
        for (i = 1; i <= nu; i++) print "  " unknown[i] > "/dev/stderr"
      }
    }
    function maxload(l, n,   s, m) { m = 0; for (s = 1; s <= n; s++) if (l[s] > m) m = l[s]; return m }
  ' |
  sort
