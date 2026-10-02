#!/usr/bin/env bash
# Tests for hack/cross-build.sh (make hack-test) on throwaway modules, so it
# runs in seconds: a clean module compiles everywhere, and one calling a
# unix-only syscall fails, naming the platforms it breaks (#128: such a
# change was green on the PR and first failed in the tag's CI). Offline;
# needs only Go.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GOPROXY=off GOWORK=off

# module <dir> <main.go>: a one-file module with no dependencies.
module() {
  mkdir -p "$1"
  printf 'module example.com/crossbuild\n\ngo 1.22\n' >"$1/go.mod"
  printf '%s\n' "$2" >"$1/main.go"
}
module "$work/clean" 'package main

import "os"

func main() { os.Exit(0) }'
module "$work/unixonly" 'package main

import (
	"os"
	"syscall"
)

func main() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }'

: >"$work/results"
# expect <name> <want-exit> <dir> <needle>: run cross-build.sh on dir.
expect() {
  local name=$1 want=$2 dir=$3 needle=$4 got=0
  hack/cross-build.sh "$dir" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

# The default set is the release's platforms (.goreleaser.yml).
expect "a clean module compiles for linux" 0 "$work/clean" "== linux/arm64"
expect "a clean module compiles for darwin" 0 "$work/clean" "== darwin/amd64"
expect "a clean module compiles for windows" 0 "$work/clean" "== windows/arm64"
expect "a unix-only syscall fails the windows/amd64 build" 1 "$work/unixonly" "::error title=cross-build::windows/amd64"
has_arm=$(grep -c "::error title=cross-build::windows/arm64" "$work/out" || true)
unix=$(grep -c "::error title=cross-build::\(linux\|darwin\)" "$work/out" || true)
if [ "$has_arm" = 1 ] && [ "$unix" = 0 ]; then
  echo "ok   every broken platform is named, and only those" | tee -a "$work/results"
else
  echo "FAIL want windows/arm64 named and no linux/darwin error:" >&2
  sed 's/^/     /' "$work/out" >&2
  echo "FAIL broken platforms" >>"$work/results"
fi

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "cross-build_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
