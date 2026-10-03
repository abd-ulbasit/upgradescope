#!/usr/bin/env bash
# Tests for hack/check-host-leak.sh (make hack-test): a package that carries
# the build machine's host name or a build path is refused. This is the leak
# that made the v0.2.0-rc.2 rpms unreproducible (nfpm wrote os.Hostname()
# into the rpm's Build Host header). Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"

expect() { # expect <name> <want-exit> <substring> <args...>
  local name=$1 want=$2 needle=$3 got=0
  shift 3
  hack/check-host-leak.sh "$@" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

# Stand-in rpms: binary header bytes around a Build Host string, the way rpm
# stores tag 1007 (NUL-terminated).
printf 'RPM\0\0header\0%s\0payload' pinned-buildhost >"$work/clean.rpm"
printf 'RPM\0\0header\0%s\0payload' runnervm8df0l.example >"$work/leaky.rpm"
printf 'RPM\0\0header\0%s\0payload' runnervm8df0l >"$work/short.rpm"
printf 'RPM\0\0header\0/tmp/build-a/dist\0payload' >"$work/path.rpm"

export UPGRADESCOPE_LEAK_HOSTNAME=runnervm8df0l.example
expect "a package without the build host passes" 0 "no build-machine names" "$work/clean.rpm"
expect "a package carrying the build host name is refused" 1 "carries the build host name 'runnervm8df0l.example'" "$work/leaky.rpm"
expect "the short host name counts too" 1 "carries the build host name 'runnervm8df0l'" "$work/short.rpm"
UPGRADESCOPE_LEAK_NAMES=/tmp/build-a expect "an extra name (a build path) is refused" 1 "carries '/tmp/build-a'" "$work/path.rpm"
UPGRADESCOPE_LEAK_NAMES=/tmp/build-a expect "an extra name absent from the package passes" 0 "no build-machine names" "$work/clean.rpm"
expect "a missing file fails" 1 "no such file" "$work/absent.rpm"
UPGRADESCOPE_LEAK_HOSTNAME=ab expect "a name under 4 characters is ignored (too common to mean anything)" 0 "no build-machine names" "$work/clean.rpm"
printf 'RPM\0\0header\0%s\0payload' upgradescope >"$work/pinned.rpm"
UPGRADESCOPE_LEAK_HOSTNAME=upgradescope expect "a host named like the pinned build host is no leak (the header is the same either way)" 0 "no build-machine names" "$work/pinned.rpm"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "check-host-leak_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
