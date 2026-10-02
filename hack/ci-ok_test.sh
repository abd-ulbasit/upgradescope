#!/usr/bin/env bash
# Tests ci.yml's ci-ok job (run by `make hack-test`). ci-ok is the one check
# a branch ruleset can require (#58): the kube job names change with the
# matrix and most jobs skip on some events, so none of them is a stable
# required check. ci-ok is only as good as its needs list, so this asserts
# it names every other job in ci.yml: a job added without it would never
# block a merge. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

ci=.github/workflows/ci.yml

# Job ids: two-space-indented keys under the top-level `jobs:`.
jobs=$(awk '/^jobs:/{j=1;next} j&&/^[^ #]/{j=0} j&&/^  [a-z0-9_-]+:[ ]*$/{sub(/^  /,"");sub(/:.*/,"");print}' "$ci" | sort)
grep -qx ci-ok <<<"$jobs" || { echo "FAIL $ci has no ci-ok job" >&2; exit 1; }

# ci-ok's needs, written as a flow list: needs: [a, b, ...] (one line).
needs=$(awk '/^  ci-ok:/{c=1;next} c&&/^  [^ ]/{c=0} c&&/^    needs:/{sub(/^    needs: *\[/,"");sub(/\].*/,"");print}' "$ci" |
  tr ',' '\n' | tr -d ' ' | sed '/^$/d' | sort)
[ -n "$needs" ] || { echo "FAIL ci-ok has no one-line needs: [...] list" >&2; exit 1; }

want=$(echo "$jobs" | grep -vx ci-ok)
missing=$(comm -23 <(echo "$want") <(echo "$needs"))
extra=$(comm -13 <(echo "$want") <(echo "$needs"))
if [ -n "$missing$extra" ]; then
  [ -z "$missing" ] || echo "FAIL ci-ok does not need: $(echo $missing)" >&2
  [ -z "$extra" ] || echo "FAIL ci-ok needs jobs that do not exist: $(echo $extra)" >&2
  exit 1
fi
echo "ok   $ci ci-ok needs every other job ($(echo "$want" | wc -l | tr -d ' '))"

# It must run when a job it needs failed or was cancelled (a plain needs
# would skip it, and a skipped required check counts as passing).
grep -q "^    if: always()" <<<"$(awk '/^  ci-ok:/{c=1;next} c&&/^  [^ ]/{c=0} c' "$ci")" ||
  { echo "FAIL ci-ok does not run with if: always()" >&2; exit 1; }
echo "ok   ci-ok runs with if: always()"
