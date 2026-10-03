#!/usr/bin/env bash
# release.yml calls ci.yml as a reusable workflow, and a called job may ask
# for no more than the caller grants: GitHub refuses to start the whole run
# otherwise (startup_failure; the v0.2.0-rc.1 tag never released because
# ci.yml's action job asks for security-events: write). This checks that
# release.yml's ci job grants every permission any ci.yml job or the
# workflow itself asks for, with at least the same access. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

ci=${CI_WORKFLOW:-.github/workflows/ci.yml}
release=${RELEASE_WORKFLOW:-.github/workflows/release.yml}

# perms <file> <job|""> : "scope access" lines of a job's permissions block
# (two-space job key, four-space permissions:, six-space scopes), or of the
# workflow's top-level block when job is empty.
perms() {
  awk -v job="$2" '
    job == "" && /^permissions:/ { p = 1; next }
    job == "" && p && /^  [a-z-]+:/ { s = $1; sub(/:$/, "", s); print s, $2; next }
    job == "" && p && /^[^ ]/ { p = 0 }
    job != "" && $0 ~ "^  " job ":[ ]*$" { j = 1; next }
    j && /^  [^ ]/ { j = 0; p = 0 }
    j && /^    permissions:/ { p = 1; next }
    j && p && /^      [a-z-]+:/ { s = $1; sub(/:$/, "", s); print s, $2; next }
    j && p && !/^      / { p = 0 }
  ' "$1"
}

jobs=$(awk '/^jobs:/{j=1;next} j&&/^[^ #]/{j=0} j&&/^  [a-z0-9_-]+:[ ]*$/{sub(/^  /,"");sub(/:.*/,"");print}' "$ci")
asked=$( { perms "$ci" ""; for j in $jobs; do perms "$ci" "$j"; done; } | sort -u)
granted=$(perms "$release" ci)

[ -n "$granted" ] || { echo "FAIL release.yml's ci job declares no permissions block" >&2; exit 1; }
rank() { case $1 in write) echo 2 ;; read) echo 1 ;; *) echo 0 ;; esac; }
fail=0
while read -r scope access; do
  [ -n "$scope" ] || continue
  have=$(awk -v s="$scope" '$1 == s { print $2 }' <<<"$granted")
  if [ "$(rank "${have:-none}")" -lt "$(rank "$access")" ]; then
    echo "FAIL ci.yml asks for $scope: $access, release.yml's ci job grants ${have:-nothing}" >&2
    fail=1
  fi
done <<<"$asked"
[ "$fail" = 0 ] || exit 1
echo "release-ci-permissions_test: release.yml's ci job grants everything ci.yml asks for ($(wc -l <<<"$asked" | tr -d ' ') scopes)"
