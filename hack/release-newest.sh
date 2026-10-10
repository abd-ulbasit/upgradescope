#!/usr/bin/env bash
# Which release is the newest, for release.yml: only the highest stable
# release moves what users follow without pinning (#244). Those are GitHub's
# Latest release (what `version: latest` and @v0 install, through
# releases/latest), the image's :latest tag and the floating major tag (v0)
# that `uses: abd-ulbasit/upgradescope@v0` resolves to. A re-run of an older
# release, or a second release racing a newer one, must leave them where
# they are: an older engine has an older knowledge base, and passes what the
# newer one blocks.
#
#   hack/release-newest.sh published
#       the tags of the published (not draft) stable releases of
#       $GITHUB_REPOSITORY, one per line, from `gh release list`
#   hack/release-newest.sh is-newest [--major] <tag>   (tags on stdin)
#       "true" when <tag> is a stable vX.Y.Z and no tag on stdin is a higher
#       stable semver, else "false". With --major, only tags of the same
#       major count: v0 follows the newest v0.x.y even once v1 exists
#   hack/release-newest.sh highest [--major vN]         (tags on stdin)
#       the highest stable tag on stdin (of major vN), or exit 1 when none
#
# Stable means vMAJOR.MINOR.PATCH with no pre-release suffix; anything else
# on stdin (v0.2.0-rc.2, v0, nightly) is ignored. Compared numerically per
# field, so v0.10.0 is higher than v0.9.9. hack/release-newest_test.sh
# (make hack-test) covers this and the release.yml steps that call it.
set -euo pipefail

usage() {
  echo "usage: $0 published | is-newest [--major] <tag> | highest [--major vN]" >&2
  exit 2
}

stable='^v[0-9]+\.[0-9]+\.[0-9]+$'

# sorted: the stable tags on stdin, highest last.
sorted() {
  { grep -E "$stable" || true; } | sed 's/^v//' | sort -t. -k1,1n -k2,2n -k3,3n | sed 's/^/v/'
}

# same <a> <b>: whether two stable tags are the same version (v0.02.0 is
# v0.2.0).
same() {
  local IFS=. a b
  read -r -a a <<<"${1#v}"
  read -r -a b <<<"${2#v}"
  ((10#${a[0]} == 10#${b[0]} && 10#${a[1]} == 10#${b[1]} && 10#${a[2]} == 10#${b[2]}))
}

case "${1:-}" in
  published)
    [ $# -eq 1 ] || usage
    : "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY names the repository}"
    gh release list -R "$GITHUB_REPOSITORY" --exclude-drafts --limit 1000 \
      --json tagName,isPrerelease --jq '.[] | select(.isPrerelease | not) | .tagName'
    ;;
  is-newest)
    shift
    major=''
    if [ "${1:-}" = --major ]; then major=1 && shift; fi
    [ $# -eq 1 ] || usage
    tag=$1
    if [[ ! $tag =~ $stable ]]; then
      cat >/dev/null
      echo false
      exit 0
    fi
    top=$({ cat; echo "$tag"; } | { if [ -n "$major" ]; then grep -E "^${tag%%.*}\." || true; else cat; fi; } | sorted | tail -n 1)
    if same "$top" "$tag"; then echo true; else echo false; fi
    ;;
  highest)
    shift
    want=''
    if [ "${1:-}" = --major ]; then
      [ $# -eq 2 ] && [[ $2 =~ ^v[0-9]+$ ]] || usage
      want=$2
    else
      [ $# -eq 0 ] || usage
    fi
    top=$({ if [ -n "$want" ]; then grep -E "^$want\." || true; else cat; fi; } | sorted | tail -n 1)
    [ -n "$top" ] || exit 1
    echo "$top"
    ;;
  *) usage ;;
esac
