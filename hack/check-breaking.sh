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
  grep -Eq '^BREAKING[ -]CHANGE:' < <(tr -d '\r' <<<"$3") || return 0
  grep -Eq '^[A-Za-z]+(\([^)]+\))?!:' <<<"$2" && return 0
  echo "::error::$1 '$2' has no '!' but its body has a BREAKING CHANGE footer. Release notes read only the subject: write it as type!: (feat!:, fix(scope)!:) so the change lands under Breaking changes." >&2
  fail=1
}

[ -z "$title" ] || check "the PR title" "$title" "$body"
if [ -n "$range" ]; then
  while IFS= read -r sha; do
    check "commit $(git rev-parse --short "$sha")" "$(git log -1 --format=%s "$sha")" "$(git log -1 --format=%b "$sha")"
  done < <(git rev-list --no-merges "$range")
fi

[ "$fail" = 0 ] || exit 1
echo "ok: every breaking change is marked with '!' in its subject"
