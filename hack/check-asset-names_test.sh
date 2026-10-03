#!/usr/bin/env bash
# Tests for hack/check-asset-names.sh (make hack-test): a release asset whose
# name GitHub would rewrite on upload is refused, because then the name in
# checksums.txt is not the name a user downloads and
# `sha256sum --ignore-missing -c checksums.txt` skips the file silently
# (v0.2.0-rc.2's deb and rpm: `~` became `.`). Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"

expect() { # expect <name> <want-exit> <substring> <checksums-lines...>
  local name=$1 want=$2 needle=$3 got=0
  shift 3
  printf '0000000000000000000000000000000000000000000000000000000000000000  %s\n' "$@" >"$work/checksums.txt"
  hack/check-asset-names.sh "$work/checksums.txt" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

expect "stable and pre-release names that need no rewriting pass" 0 "ok: 5 asset names" \
  upgradescope_linux_amd64.tar.gz upgradescope_windows_arm64.zip upgradescope_0.2.0.rc.2_amd64.deb \
  upgradescope-0.2.0.rc.2-1.x86_64.rpm upgradescope_0.2.0_rc.2_x86_64.apk
expect "a tilde (nfpm's pre-release form) is refused" 1 "'upgradescope_0.2.0~rc.2_amd64.deb' would be renamed by GitHub" \
  upgradescope_linux_amd64.tar.gz upgradescope_0.2.0~rc.2_amd64.deb
expect "a plus is refused too" 1 "would be renamed by GitHub" upgradescope_0.2.0+build_amd64.deb
expect "the contracts pass" 0 "ok: 3 asset names" report.schema.json webhook.schema.json openapi.yaml

if hack/check-asset-names.sh "$work/absent.txt" >"$work/out" 2>&1; then
  echo "FAIL a missing checksums.txt fails" >&2; echo "FAIL a missing checksums.txt fails" >>"$work/results"
else
  echo "ok   a missing checksums.txt fails" | tee -a "$work/results"
fi

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "check-asset-names_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
