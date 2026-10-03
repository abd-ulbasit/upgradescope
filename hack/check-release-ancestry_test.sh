#!/usr/bin/env bash
# Tests for hack/check-release-ancestry.sh (make hack-test): a release tag is
# accepted only on a commit that is on main. Scratch repositories, offline.
set -euo pipefail
cd "$(dirname "$0")/.."
script="$PWD/hack/check-release-ancestry.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"

# A "remote" with main (c1, c2) and a branch off c1 (side) that never merged.
git init -q --bare -b main "$work/remote.git"
git clone -q "$work/remote.git" "$work/seed" 2>/dev/null
(
  cd "$work/seed"
  git config user.email t@example.com
  git config user.name t
  git config commit.gpgsign false
  git symbolic-ref HEAD refs/heads/main # whatever init.defaultBranch says
  git commit -q --allow-empty -m c1
  git commit -q --allow-empty -m c2
  git push -q origin main
  git checkout -q -b side HEAD~1
  git commit -q --allow-empty -m on-side
  git tag v0.2.0-side
  git push -q origin side v0.2.0-side
  git tag v0.2.0-main main
  git push -q origin v0.2.0-main
)

expect() { # expect <name> <want-exit> <substring> <clone-dir> <ref> [env...]
  local name=$1 want=$2 needle=$3 dir=$4 ref=$5 got=0
  shift 5
  (cd "$dir" && env "$@" "$script" "$ref") >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

git clone -q "$work/remote.git" "$work/full" 2>/dev/null
(cd "$work/full" && git fetch -q --tags origin side)
expect "a tag on main's tip passes" 0 "is on origin/main" "$work/full" v0.2.0-main
expect "a tag on an older main commit passes" 0 "is on origin/main" "$work/full" origin/main~1
expect "a tag on a branch that never reached main is refused" 1 "is not on origin/main" "$work/full" v0.2.0-side
expect "HEAD is the default ref" 0 "is on origin/main" "$work/full" HEAD

# What actions/checkout gives a tag push without fetch-depth: 0: one commit,
# no origin/main. That must fail loudly, not pass.
git clone -q --depth 1 --branch v0.2.0-main "file://$work/remote.git" "$work/shallow" 2>/dev/null
expect "a shallow checkout with no origin/main is refused, naming the fix" 1 "fetch-depth: 0" "$work/shallow" HEAD

git clone -q --depth 1 "file://$work/remote.git" "$work/shallow-main" 2>/dev/null
expect "a shallow clone that has origin/main is refused too (its history is cut)" 1 "is shallow" "$work/shallow-main" HEAD

expect "the main ref is configurable" 0 "is on main" "$work/full" v0.2.0-main UPGRADESCOPE_MAIN_REF=main
expect "a tag name that is not a ref fails" 1 "not a commit" "$work/full" v9.9.9

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "check-release-ancestry_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
