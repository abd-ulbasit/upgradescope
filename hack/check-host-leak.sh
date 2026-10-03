#!/usr/bin/env bash
# Fails when a release package carries the build machine's host name or a
# build path (hack/test-release-repro.sh; #198). A package that embeds either
# cannot be rebuilt byte for byte on another machine: nfpm wrote the rpm's
# Build Host header from os.Hostname() until .goreleaser.yml pinned
# nfpms[].rpm.buildhost, so the v0.2.0-rc.2 rpms differed from any rebuild.
# Comparing two builds on one machine cannot see that, hence this scan.
#
# Usage: hack/check-host-leak.sh <package>...
# Scans for this machine's host name (full and short) and each newline-
# separated name in UPGRADESCOPE_LEAK_NAMES (build paths). Names under four
# characters are ignored: they match too much to mean anything, and so is a
# name equal to the pinned build host. A generic host name such as localhost
# can still match package text, and fails the scan: a raw substring match
# cannot tell an rpm header from file content.
# UPGRADESCOPE_LEAK_HOSTNAME replaces `hostname` (hack/check-host-leak_test.sh).
# Offline.
set -euo pipefail

die() { echo "::error::host-leak: $*" >&2; exit 1; }
[ "$#" -gt 0 ] || die "usage: $0 <package>..."

pinned=upgradescope # nfpms[].rpm.buildhost in .goreleaser.yml
host=${UPGRADESCOPE_LEAK_HOSTNAME:-$(hostname)}
names=$(
  printf '%s\n' "$host" "${host%%.*}"
  printf '%s\n' "${UPGRADESCOPE_LEAK_NAMES:-}"
)

for f in "$@"; do
  [ -f "$f" ] || die "$f: no such file"
  while IFS= read -r name; do
    [ "${#name}" -ge 4 ] || continue
    # A host named like the pinned build host writes the same header anyway.
    [ "$name" != "$pinned" ] || continue
    if grep -aqF -- "$name" "$f"; then
      if [ "$name" = "$host" ] || [ "$name" = "${host%%.*}" ]; then
        die "$f carries the build host name '$name': pin it (.goreleaser.yml nfpms[].rpm.buildhost) so a rebuild elsewhere matches"
      fi
      die "$f carries '$name', a path of the build machine, so a rebuild elsewhere would differ"
    fi
  done <<<"$names"
done
echo "ok: no build-machine names in $# package(s)"
