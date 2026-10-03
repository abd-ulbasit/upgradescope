#!/usr/bin/env bash
# Tests for hack/check-breaking.sh (make hack-test): a PR title or commit
# whose body has a BREAKING CHANGE footer but whose subject has no `!` is
# refused. GoReleaser groups release notes by the subject alone, so a footer-
# only break would land under Features or Bug fixes (#198, IR-06). Offline;
# the commit cases run in a scratch repository.
set -euo pipefail
cd "$(dirname "$0")/.."
script="$PWD/hack/check-breaking.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"

expect() { # expect <name> <want-exit> <substring> -- <env assignments and args for the script>
  local name=$1 want=$2 needle=$3 got=0
  shift 3
  env "$@" "$script" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

# --- the PR title and body ---------------------------------------------------
expect "a plain title and body pass" 0 "ok" \
  PR_TITLE='fix(agent): bound the queue' PR_BODY=$'Why.\n\nFixes #1'
expect "a '!' title with a BREAKING CHANGE body passes" 0 "ok" \
  PR_TITLE='feat(cli)!: drop --legacy' PR_BODY=$'Why.\n\nBREAKING CHANGE: --legacy is gone'
expect "a scoped '!' title passes" 0 "ok" \
  PR_TITLE='chore(deps)!: bump k8s.io/api' PR_BODY=$'BREAKING CHANGE: needs Go 1.27'
expect "a BREAKING CHANGE body without '!' in the title is refused" 1 "the PR title 'feat: new scoring' has no '!'" \
  PR_TITLE='feat: new scoring' PR_BODY=$'Why.\n\nBREAKING CHANGE: scores shift'
expect "the hyphenated BREAKING-CHANGE footer counts" 1 "has no '!'" \
  PR_TITLE='fix: something' PR_BODY=$'BREAKING-CHANGE: x'
expect "a CRLF body (the GitHub web form) counts" 1 "has no '!'" \
  PR_TITLE='fix: something' PR_BODY=$'Why.\r\n\r\nBREAKING CHANGE: x\r\n'
expect "a title with no type cannot carry the break either" 1 "has no '!'" \
  PR_TITLE='Update the thing' PR_BODY=$'BREAKING CHANGE: x'
expect "an uppercase type is refused: the release-notes group matches lowercase only" 1 "type must be lowercase" \
  PR_TITLE='Feat!: new scoring' PR_BODY=$'BREAKING CHANGE: scores shift'
expect "the footer must start a line (prose mentioning it is not a footer)" 0 "ok" \
  PR_TITLE='docs: explain the rule' PR_BODY='Say "BREAKING CHANGE: x" in the footer'

# --- commits -----------------------------------------------------------------
repo="$work/repo"
git init -q "$repo"
(
  cd "$repo"
  git config user.email t@example.com
  git config user.name t
  git config commit.gpgsign false
  git commit -q --allow-empty -m 'chore: base'
  git tag base
  git commit -q --allow-empty -m 'feat!: dropped' -m 'BREAKING CHANGE: gone'
  git commit -q --allow-empty -m 'fix: small' -m 'Why. Refs #1'
)

got=0
(cd "$repo" && COMMIT_RANGE=base..HEAD "$script" >"$work/out" 2>&1) || got=$?
if [ "$got" = 0 ] && grep -qF ok "$work/out"; then
  echo "ok   commits with '!' (or no footer) pass" | tee -a "$work/results"
else
  echo "FAIL commits with '!' (or no footer) pass: exit $got" >&2; sed 's/^/     /' "$work/out" >&2
  echo "FAIL commits with '!' (or no footer) pass" >>"$work/results"
fi

(cd "$repo" && git commit -q --allow-empty -m 'feat: new scoring' -m $'Why.\n\nBREAKING CHANGE: scores shift')
bad=$(cd "$repo" && git rev-parse --short HEAD)
got=0
(cd "$repo" && COMMIT_RANGE=base..HEAD "$script" >"$work/out" 2>&1) || got=$?
if [ "$got" = 1 ] && grep -qF "commit $bad 'feat: new scoring' has no '!'" "$work/out"; then
  echo "ok   a footer-only break in a commit is refused, naming the commit" | tee -a "$work/results"
else
  echo "FAIL a footer-only break in a commit is refused: exit $got" >&2; sed 's/^/     /' "$work/out" >&2
  echo "FAIL a footer-only break in a commit is refused" >>"$work/results"
fi

# A merge commit (a PR merged into the branch by hand) is not judged.
(cd "$repo" && git checkout -q -b side base && git commit -q --allow-empty -m 'fix: side' \
  && git checkout -q - && git merge -q --no-ff side -m 'Merge branch side' -m 'BREAKING CHANGE: merge text')
got=0
(cd "$repo" && COMMIT_RANGE=base..HEAD "$script" >"$work/out" 2>&1) || got=$?
if [ "$got" = 1 ] && ! grep -qF 'Merge branch' "$work/out"; then
  echo "ok   merge commits are skipped" | tee -a "$work/results"
else
  echo "FAIL merge commits are skipped: exit $got" >&2; sed 's/^/     /' "$work/out" >&2
  echo "FAIL merge commits are skipped" >>"$work/results"
fi

# An uppercase type with `!` is still not Breaking changes in the release notes.
(cd "$repo" && git commit -q --allow-empty -m 'Chore!: x' -m 'BREAKING CHANGE: y')
upper=$(cd "$repo" && git rev-parse --short HEAD)
got=0
(cd "$repo" && COMMIT_RANGE=base..HEAD "$script" >"$work/out" 2>&1) || got=$?
if [ "$got" = 1 ] && grep -qF "commit $upper 'Chore!: x'" "$work/out"; then
  echo "ok   an uppercase-type commit is refused, naming the commit" | tee -a "$work/results"
else
  echo "FAIL an uppercase-type commit is refused: exit $got" >&2; sed 's/^/     /' "$work/out" >&2
  echo "FAIL an uppercase-type commit is refused" >>"$work/results"
fi

expect "no input at all fails (a misconfigured workflow must not pass)" 1 "nothing to check" \
  PR_TITLE= PR_BODY= COMMIT_RANGE=

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "check-breaking_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
