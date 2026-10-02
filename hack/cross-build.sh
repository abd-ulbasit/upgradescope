#!/usr/bin/env bash
# Compiles every package, tests included (go build + go vet), for every
# platform the release ships (make cross-build; a step of CI's build job).
# Unit tests, lint and the build otherwise run on linux only, so a change
# that breaks only windows (a unix-only syscall) or darwin was green on the
# PR and first failed in the tag's own CI (#128). CGO_ENABLED=0, as
# .goreleaser.yml builds. A few minutes cold, well under one with the Go
# build cache warm.
#
# Usage: hack/cross-build.sh [module dir]   (default: the repository root)
# Knob: CROSS_BUILD_PLATFORMS (default: the .goreleaser.yml goos x goarch).
set -euo pipefail
cd "$(dirname "$0")/.."

dir=${1:-.}
platforms=${CROSS_BUILD_PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}

broken=()
for p in $platforms; do
  echo "== $p"
  if ! (cd "$dir" && export CGO_ENABLED=0 GOOS="${p%/*}" GOARCH="${p#*/}" && go build ./... && go vet ./...); then
    echo "::error title=cross-build::$p does not compile (see above)"
    broken+=("$p")
  fi
done
if [ "${#broken[@]}" -gt 0 ]; then
  echo "cross-build: broken on ${broken[*]}" >&2
  exit 1
fi
echo "cross-build: OK ($platforms)"
