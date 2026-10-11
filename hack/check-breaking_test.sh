#!/usr/bin/env bash
# Tests for hack/check-breaking.sh (make hack-test): a PR title or commit
# whose body has a BREAKING CHANGE footer but whose subject has no `!` is
# refused, and so is a subject with an uppercase or mixed-case type (`Feat:`,
# `Chore!:`, `FIX(scope):`), footer or not. GoReleaser groups release notes by
# the subject alone and matches lowercase types only, so a footer-only break
# would land under Features or Bug fixes, and an uppercase type under Other
# changes (#198, #388, IR-06). Offline; the commit cases run in a scratch
# repository.
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
# An uppercase or mixed-case type is flagged with or without a footer (#388):
# the release-notes groups match lowercase types only.
expect "an uppercase type without a footer is refused" 1 "the PR title 'Feat: add a flag' has an uppercase type" \
  PR_TITLE='Feat: add a flag' PR_BODY='Why.'
expect "an uppercase type with '!' and no footer is refused" 1 "the PR title 'Chore!: drop it' has an uppercase type" \
  PR_TITLE='Chore!: drop it' PR_BODY='Why.'
expect "a fully uppercase scoped type is refused" 1 "the PR title 'FIX(scope): bound the queue' has an uppercase type" \
  PR_TITLE='FIX(scope): bound the queue' PR_BODY=
expect "a mixed-case type is refused" 1 "the PR title 'FeAt(cli): x' has an uppercase type" \
  PR_TITLE='FeAt(cli): x' PR_BODY=
expect "the uppercase message says what to write" 1 "The type must be lowercase" \
  PR_TITLE='Fix: x' PR_BODY=
expect "an uppercase type with a footer and no '!' names both problems" 1 "write it as type!:" \
  PR_TITLE='Feat: new scoring' PR_BODY=$'BREAKING CHANGE: scores shift'
expect "a lowercase type without a footer still passes" 0 "ok" \
  PR_TITLE='feat(cli): add a flag' PR_BODY='Why.'
expect "a title with a capital but no type is not a type" 0 "ok" \
  PR_TITLE='Update the README: wording' PR_BODY=
expect "a revert of a quoted subject is not judged" 0 "ok" \
  PR_TITLE='Revert "feat: add a flag"' PR_BODY=
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

# An uppercase type needs no footer to be flagged (#388), in commits too.
(cd "$repo" && git commit -q --allow-empty -m 'Feat: plain' -m 'Why.')
plain=$(cd "$repo" && git rev-parse --short HEAD)
got=0
(cd "$repo" && COMMIT_RANGE=base..HEAD "$script" >"$work/out" 2>&1) || got=$?
if [ "$got" = 1 ] && grep -qF "commit $plain 'Feat: plain' has an uppercase type" "$work/out"; then
  echo "ok   an uppercase-type commit without a footer is refused, naming the commit" | tee -a "$work/results"
else
  echo "FAIL an uppercase-type commit without a footer is refused: exit $got" >&2; sed 's/^/     /' "$work/out" >&2
  echo "FAIL an uppercase-type commit without a footer is refused" >>"$work/results"
fi

# Only the uppercase commits fail: in a range of just lowercase ones it passes.
(cd "$repo" && git checkout -q -b clean base && git commit -q --allow-empty -m 'fix(x): a' -m 'Why.' \
  && git commit -q --allow-empty -m 'Update the docs: wording')
got=0
(cd "$repo" && COMMIT_RANGE=base..HEAD "$script" >"$work/out" 2>&1) || got=$?
if [ "$got" = 0 ] && grep -qF ok "$work/out"; then
  echo "ok   lowercase types and untyped subjects pass in a commit range" | tee -a "$work/results"
else
  echo "FAIL lowercase types and untyped subjects pass in a commit range: exit $got" >&2; sed 's/^/     /' "$work/out" >&2
  echo "FAIL lowercase types and untyped subjects pass in a commit range" >>"$work/results"
fi
(cd "$repo" && git checkout -q -)

# An unresolvable range must fail, not check zero commits and pass: a base.sha
# the checkout cannot see would otherwise turn the lint into a silent pass.
got=0
(cd "$repo" && COMMIT_RANGE=deadbeefdeadbeef..HEAD "$script" >"$work/out" 2>&1) || got=$?
if [ "$got" != 0 ] && ! grep -qF 'ok:' "$work/out" && grep -qF 'cannot read COMMIT_RANGE' "$work/out"; then
  echo "ok   an unresolvable COMMIT_RANGE fails" | tee -a "$work/results"
else
  echo "FAIL an unresolvable COMMIT_RANGE fails: exit $got" >&2; sed 's/^/     /' "$work/out" >&2
  echo "FAIL an unresolvable COMMIT_RANGE fails" >>"$work/results"
fi

expect "no input at all fails (a misconfigured workflow must not pass)" 1 "nothing to check" \
  PR_TITLE= PR_BODY= COMMIT_RANGE=

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "check-breaking_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
