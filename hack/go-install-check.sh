#!/usr/bin/env bash
# What a `go install` user gets (make go-install-check; CI's build job).
# The README offers `go install github.com/abd-ulbasit/upgradescope/cmd/
# upgradescope@latest`; this builds the same way from the checkout and
# asserts the two claims that once failed for v0.1.1 (#127):
#   1. `upgradescope version` names a real version, not "dev": the go
#      command stamps the module version (the tag, or a pseudo-version) and
#      the commit; on a tagged commit it must be that tag;
#   2. `serve` serves the dashboard at / with every asset it references
#      (internal/server/webdist is committed, so go install embeds it).
# Needs Go, git, jq and curl, in a normal clone: in a `git worktree` the
# go command's VCS stamping does not find the worktree's repository (its
# .git is a file), so it stamps an enclosing repository's HEAD or nothing,
# and check 1 fails although the build is fine.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "::error::go-install-check: $*" >&2; exit 1; }
command -v jq >/dev/null || die "jq is required (brew install jq / apt install jq)"

gobin=$(mktemp -d)
trap 'rm -rf "$gobin"' EXIT
GOBIN="$gobin" go install ./cmd/upgradescope
bin="$gobin/upgradescope"
"$bin" version

info="$("$bin" version --output json)"
v="$(jq -r .version <<<"$info")"
case "$v" in
  '' | dev | dev+*) die "go install build reports version '$v'; the go command did not stamp a module version" ;;
esac
head="$(git rev-parse HEAD)"
[ "$(jq -r .commit <<<"$info")" = "$head" ] || die "version reports commit $(jq -r .commit <<<"$info"), want HEAD $head"
if tag="$(git describe --tags --exact-match --match 'v[0-9]*' HEAD 2>/dev/null)" && git diff --quiet HEAD; then
  [ "$v" = "${tag#v}" ] || die "HEAD is tagged $tag but go install reports version '$v'"
fi
echo "ok: go install reports version $v at $head"

DASHBOARD_SMOKE_PORT=18432 hack/dashboard-smoke.sh "$bin"
echo "go-install-check: OK"
