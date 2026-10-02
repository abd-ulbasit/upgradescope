#!/usr/bin/env bash
# Keeps docs/claims.md honest (make claims-check; CI's test job): every
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

rows=0 refs=0 bad=0
while IFS= read -r line; do
  [[ "$line" == '|'* ]] || continue
  [[ "$line" =~ ^\|[-:\ \|]+$ ]] && continue # header separator
  id=$(cut -d'|' -f2 <<<"$line" | xargs)
  [ "$id" != ID ] || continue # header
  rows=$((rows + 1))
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
[ "$bad" = 0 ] || { echo "claims-check: $bad dangling reference(s) in $file" >&2; exit 1; }
echo "claims-check: $rows claims, $refs references, all resolve"
