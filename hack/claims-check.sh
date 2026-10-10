#!/usr/bin/env bash
# Keeps docs/claims.md honest (make claims-check; CI's repo-checks job): every
# reference in a claim row's last column ("Proven by") must still exist, so
# renaming or deleting the test behind a public claim fails CI instead of
# leaving the ledger pointing at nothing. The references, each backticked:
#
#   `TestName`     a Go test function in any module of the repository
#   `e2e:fn`       a function hack/e2e.sh runs as a step (defined and called)
#   `ci:job`       a job of .github/workflows/ci.yml
#   `make target`  a Makefile target
#   `some/path`    a file or directory in the repository
#
# Anything else in backticks there is an error, and every claim row must name
# at least one reference or a tracking issue (#N): "not automated: #99".
#
# A claim ID names one claim: an ID in the first column of two rows fails,
# wherever they are in the ledger, so a new claim cannot take an ID already
# given (the audit's, or another branch's). A row may list several IDs,
# separated by commas, and a range ("IR-07 to IR-09" is IR-07, IR-08 and
# IR-09); each of them counts.
#
# Knob: CLAIMS_FILE (default docs/claims.md).
set -euo pipefail
cd "$(dirname "$0")/.."

file=${CLAIMS_FILE:-docs/claims.md}
[ -f "$file" ] || { echo "claims-check: no $file" >&2; exit 1; }

# The repository's test files, tracked or new, as CI checks them out: not
# node_modules, nor other branches' worktrees under .claude/.
go_tests=$(git ls-files -z --cached --others --exclude-standard -- '*_test.go' |
  xargs -0 grep -hoE '^func Test[A-Za-z0-9_]+\(' | sed 's/^func //; s/($//' | sort -u)
ci_jobs=$(awk '/^jobs:/{j=1;next} j&&/^[^ #]/{j=0} j&&/^  [a-z0-9_-]+:[ ]*$/{sub(/^  /,"");sub(/:.*/,"");print}' .github/workflows/ci.yml)

# ref_error <ref>: why a reference does not resolve (empty when it does).
ref_error() {
  local r=$1
  case "$r" in
    Test*)
      [[ "$r" =~ ^Test[A-Za-z0-9_]+$ ]] && grep -qxF "$r" <<<"$go_tests" || echo "no Go test $r" ;;
    e2e:*)
      local fn=${r#e2e:}
      # Defined, and run as a step: the command of a gate/best_effort call,
      # or an entry of the audit_checks list ("<name>|<fn>").
      { grep -qE "^$fn\(\) \{" hack/e2e.sh &&
        grep -qE "^[[:space:]]*(gate|best_effort) .* $fn\$|\|$fn\"\$" hack/e2e.sh; } ||
        echo "hack/e2e.sh has no gate $fn" ;;
    ci:*)
      grep -qxF "${r#ci:}" <<<"$ci_jobs" || echo ".github/workflows/ci.yml has no job ${r#ci:}" ;;
    make\ *)
      grep -qE "^${r#make }:" Makefile || echo "the Makefile has no target ${r#make }" ;;
    */*)
      [[ "$r" != *' '* ]] || { echo "unknown reference '$r'"; return; }
      [ -e "$r" ] || echo "no file $r" ;;
    *) echo "unknown reference '$r'" ;;
  esac
}

# row_ids <first column>: the claim IDs a row names, one a line, ranges
# expanded ("IR-07 to IR-09": IR-07, IR-08, IR-09, at the first's width).
row_ids() {
  local t prefix from to width n
  while IFS= read -r t; do
    t=$(xargs <<<"$t")
    [ -n "$t" ] || continue
    if [[ "$t" =~ ^([A-Z]+)-([0-9]+)\ to\ ([A-Z]+)-([0-9]+)$ ]] && [ "${BASH_REMATCH[1]}" = "${BASH_REMATCH[3]}" ]; then
      prefix=${BASH_REMATCH[1]} from=${BASH_REMATCH[2]} to=${BASH_REMATCH[4]} width=${#BASH_REMATCH[2]}
      for ((n = 10#$from; n <= 10#$to; n++)); do printf '%s-%0*d\n' "$prefix" "$width" "$n"; done
    else
      echo "$t"
    fi
  done < <(tr ',' '\n' <<<"$1")
}

rows=0 refs=0 bad=0 seen="" n=0 in_table=0
while IFS= read -r line; do
  n=$((n + 1))
  # Inside a table every non-blank line must be a row: text that lands on a
  # line of its own (a split row, or a sentence pasted in front of one)
  # would otherwise be skipped, and so would the claim it carries.
  if [[ "$line" != '|'* ]]; then
    if [ "$in_table" = 1 ] && [ -n "${line//[[:space:]]/}" ]; then
      echo "claims-check: $file:$n: a line inside a claims table does not start with '|': ${line:0:80}" >&2
      bad=$((bad + 1))
    fi
    in_table=0
    continue
  fi
  in_table=1
  [[ "$line" =~ ^\|[-:\ \|]+$ ]] && continue # header separator
  id=$(cut -d'|' -f2 <<<"$line" | xargs)
  [ "$id" != ID ] || continue # header
  if [ -z "$id" ]; then
    echo "claims-check: $file:$n: a claim row with no ID: ${line:0:80}" >&2
    bad=$((bad + 1))
    continue
  fi
  rows=$((rows + 1))
  while IFS= read -r one; do
    if grep -qxF -- "$one" <<<"$seen"; then
      echo "claims-check: $file: $one: the ID of more than one claim row" >&2
      bad=$((bad + 1))
    fi
    seen+="$one"$'\n'
  done < <(row_ids "$id")
  proof=${line%|}
  proof=${proof##*|}
  found=0
  while IFS= read -r r; do
    [ -n "$r" ] || continue
    found=1
    refs=$((refs + 1))
    why=$(ref_error "$r")
    if [ -n "$why" ]; then
      echo "claims-check: $file: $id: $why" >&2
      bad=$((bad + 1))
    fi
  done < <(grep -oE '`[^`]+`' <<<"$proof" | tr -d '`' || true)
  if [ "$found" = 0 ] && ! grep -qE '#[0-9]+' <<<"$proof"; then
    echo "claims-check: $file: $id: names no test and no tracking issue" >&2
    bad=$((bad + 1))
  fi
done <"$file"

[ "$rows" -gt 0 ] || { echo "claims-check: $file has no claim rows (| ID | Claim | Proven by |)" >&2; exit 1; }
[ "$bad" = 0 ] || { echo "claims-check: $bad dangling reference(s) or duplicate ID(s) in $file" >&2; exit 1; }
echo "claims-check: $rows claims, $refs references, all resolve"
