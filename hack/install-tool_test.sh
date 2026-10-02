#!/usr/bin/env bash
# Tests for hack/install-tool.sh. Offline: every download is redirected to a
# local file:// URL, while the expected checksum stays the pinned one, so the
# cases prove that bytes which are not the pinned release are never
# installed.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
printf 'not kind\n' >"$work/junk"

: >"$work/results"
# expect <name> <want-exit> <want-substring> <tool> [env...]
expect() {
  local name=$1 want=$2 needle=$3 tool=$4 got=0
  shift 4
  env TOOLS_BIN="$work/bin" "$@" hack/install-tool.sh "$tool" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

expect "unknown tool is rejected" 2 "unknown tool 'helmfile'" helmfile
expect "unsupported platform is rejected" 2 "no pinned kind for plan9/amd64" kind \
  UPGRADESCOPE_TOOL_PLATFORM=plan9/amd64
expect "checksum mismatch fails" 1 "sha256 mismatch for kind" kind \
  UPGRADESCOPE_TOOL_URL="file://$work/junk"
if [ -e "$work/bin/kind" ]; then
  echo "FAIL checksum mismatch left $work/bin/kind behind" | tee -a "$work/results" >&2
else
  echo "ok   checksum mismatch installs nothing" | tee -a "$work/results"
fi
expect "failed download fails" 1 "could not download kubectl" kubectl \
  UPGRADESCOPE_TOOL_URL="file://$work/does-not-exist"
expect "tarball checksum mismatch fails" 1 "sha256 mismatch for kubeconform" kubeconform \
  UPGRADESCOPE_TOOL_URL="file://$work/junk"
expect "oras tarball checksum mismatch fails" 1 "sha256 mismatch for oras" oras \
  UPGRADESCOPE_TOOL_URL="file://$work/junk"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "install-tool_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
