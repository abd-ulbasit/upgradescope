#!/usr/bin/env bash
# A change that says BREAKING CHANGE in a body must say `!` in its subject
# (.github/workflows/pr-lint.yml; #198, IR-06). GoReleaser builds the release
# notes from commit subjects alone, so `feat: new scoring` with a
# `BREAKING CHANGE:` footer would land under Features instead of Breaking
# changes. CONTRIBUTING.md, "Commit conventions", states the rule.
#
# Checks, each only when given:
#   PR_TITLE, PR_BODY  the pull request's title and description (the squash
#                      merge's subject and body)
#   COMMIT_RANGE       a git revision range (base..head); the subject and body
#                      of each non-merge commit in it, run from inside the repo
# A footer is a line that starts with `BREAKING CHANGE:` or `BREAKING-CHANGE:`
# (Conventional Commits). The subject must then be `type!:` or `type(scope)!:`.
# Separately, with or without a footer, a subject whose type is uppercase or
# mixed-case (`Feat:`, `Chore!:`, `FIX(scope):`) is refused: the release-notes
# groups match lowercase types only, so it would land under Other changes
# (#388). A subject with no type (`Update the docs`) is not judged.
# Offline; hack/check-breaking_test.sh drives it.
set -euo pipefail

title=${PR_TITLE:-}
body=${PR_BODY:-}
range=${COMMIT_RANGE:-}
if [ -z "$title" ] && [ -z "$range" ]; then
  echo "::error::check-breaking: nothing to check (neither PR_TITLE nor COMMIT_RANGE is set)" >&2
  exit 1
fi

fail=0
# check <what> <subject> <body>
check() {
  local what=$1 subject=$2 hasfooter=0
  grep -Eq '^BREAKING[ -]CHANGE:' < <(tr -d '\r' <<<"$3") && hasfooter=1
  # The type is the word before the optional (scope), optional ! and the colon.
  # A type with an uppercase letter anywhere (Feat, FIX, FeAt) never reaches
  # a release-notes group, whose patterns are in .goreleaser.yml.
  if grep -Eq '^[A-Za-z]*[A-Z][A-Za-z]*(\([^)]+\))?!?:' <<<"$subject"; then
    local why="has an uppercase type"
    if [ "$hasfooter" = 1 ] && ! grep -Eq '^[A-Za-z]+(\([^)]+\))?!:' <<<"$subject"; then
      why="$why and no '!' but its body has a BREAKING CHANGE footer (write it as type!:)"
    elif [ "$hasfooter" = 1 ]; then
      why="$why but its body has a BREAKING CHANGE footer"
    fi
    echo "::error::$what '$subject' $why. The type must be lowercase: release notes match lowercase types only, so this lands under Other changes. Write feat:, fix(scope):, or feat!: for a breaking change." >&2
    fail=1
    return 0
  fi
  [ "$hasfooter" = 1 ] || return 0
  # The same pattern as the Breaking changes group in .goreleaser.yml.
  grep -Eq '^[a-z]+(\([^)]+\))?!:' <<<"$subject" && return 0
  echo "::error::$what '$subject' has no '!' but its body has a BREAKING CHANGE footer. Release notes read only the subject: write it as type!: (feat!:, fix(scope)!:) so the change lands under Breaking changes." >&2
  fail=1
}

[ -z "$title" ] || check "the PR title" "$title" "$body"
if [ -n "$range" ]; then
  # Resolve the list in a plain assignment: a failure inside `done < <(...)` is
  # invisible to `set -e`, and an unresolvable range would check zero commits
  # and print the success message.
  commits=$(git rev-list --no-merges "$range") || {
    echo "::error::check-breaking: cannot read COMMIT_RANGE '$range' (does the checkout have the base commit? fetch-depth: 0)" >&2
    exit 1
  }
  while IFS= read -r sha; do
    [ -n "$sha" ] || continue
    check "commit $(git rev-parse --short "$sha")" "$(git log -1 --format=%s "$sha")" "$(git log -1 --format=%b "$sha")"
  done <<<"$commits"
fi

[ "$fail" = 0 ] || exit 1
echo "ok: every breaking change is marked with '!' in its subject, and no type is uppercase"
