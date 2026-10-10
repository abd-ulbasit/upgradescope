#!/usr/bin/env bash
# Tests for hack/check-toolchain.sh (run by `make check-toolchain`). Each case
# copies a minimal fixture tree (go.mod, Dockerfiles, Makefile, release.yml),
# breaks one thing, and asserts the check's exit status and message. Offline:
# the goreleaser go.mod lookup through the module proxy is switched off.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# fixture <dir>: a tree that passes.
fixture() {
  local d=$1
  mkdir -p "$d/.github/workflows"
  printf 'module example.com/x\n\ngo 1.26.8\n' >"$d/go.mod"
  cat >"$d/Dockerfile" <<'EOF'
FROM --platform=$BUILDPLATFORM node:24-alpine@sha256:aa AS web
FROM --platform=$BUILDPLATFORM golang:1.26.8@sha256:bb AS build
FROM gcr.io/distroless/static-debian12:nonroot@sha256:cc
EOF
  printf 'FROM gcr.io/distroless/static-debian12:nonroot@sha256:cc\n' >"$d/Dockerfile.release"
  printf 'GORELEASER_VERSION ?= v2.17.1\n' >"$d/Makefile"
  cat >"$d/.github/workflows/release.yml" <<'EOF'
jobs:
  binaries:
    steps:
      - uses: goreleaser/goreleaser-action@f06c13b6b1a9625abc9e6e439d9c05a8f2190e94 # v7.2.3
        with:
          version: v2.17.1 # GORELEASER_VERSION in the Makefile; make check-toolchain compares them
          args: build
      - uses: goreleaser/goreleaser-action@f06c13b6b1a9625abc9e6e439d9c05a8f2190e94 # v7.2.3
        with:
          version: v2.17.1 # GORELEASER_VERSION in the Makefile; make check-toolchain compares them
          args: release --clean
EOF
  cat >"$d/.github/workflows/ci.yml" <<'YAML'
jobs:
  test:
    steps:
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
YAML
}

: >"$work/results"
# expect <name> <want-exit> <want-substring> <fixture-dir>
expect() {
  local name=$1 want=$2 needle=$3 dir=$4 got=0
  UPGRADESCOPE_TOOLCHAIN_ROOT="$dir" UPGRADESCOPE_TOOLCHAIN_OFFLINE=1 \
    hack/check-toolchain.sh >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

# case_dir <name>: a fresh passing fixture to break; edit <sed-expr> <file>
# edits in place portably (BSD and GNU sed) and leaves no backup behind
# for the Dockerfile* glob to pick up.
case_dir() { local d="$work/$1"; fixture "$d"; echo "$d"; }
edit() { sed -i.bak "$1" "$2" && rm -f "$2.bak"; }

d=$(case_dir setup-go-literal)
edit 's#go-version-file: go.mod#go-version: '"'"'1.26.x'"'"'#' "$d/.github/workflows/ci.yml"
expect "a literal setup-go version fails" 1 "pins Go literally instead of reading go.mod" "$d"

d=$(case_dir setup-go-missing)
edit '/go-version-file: go.mod/d' "$d/.github/workflows/ci.yml"
expect "a setup-go step without go-version-file fails" 1 "has 1 setup-go steps but 0 read 'go-version-file: go.mod'" "$d"

d=$(case_dir setup-go-ok)
expect "setup-go reading go.mod passes" 0 "ok: .github/workflows/ci.yml: 1 setup-go step(s) read go.mod" "$d"

d=$(case_dir pass)
expect "consistent tree passes" 0 "ok: Dockerfile golang:1.26.8 == go.mod go 1.26.8" "$d"

d=$(case_dir plain-from)
edit 's#^FROM --platform=\$BUILDPLATFORM golang:#FROM golang:#' "$d/Dockerfile"
expect "FROM without --platform is read too" 0 "ok: Dockerfile golang:1.26.8" "$d"

d=$(case_dir old-golang)
edit 's#golang:1.26.8#golang:1.26.7#' "$d/Dockerfile"
expect "Dockerfile behind go.mod fails" 1 "Dockerfile builds on golang:1.26.7 but go.mod requires go 1.26.8" "$d"

d=$(case_dir new-gomod)
printf 'module example.com/x\n\ngo 1.26.9\n' >"$d/go.mod"
expect "go.mod ahead of the Dockerfile fails" 1 "go.mod requires go 1.26.9" "$d"

d=$(case_dir no-golang)
edit '/golang:/d' "$d/Dockerfile"
expect "Dockerfile with no golang stage fails" 1 "Dockerfile has no 'FROM golang:<version>' line" "$d"

d=$(case_dir no-go-directive)
printf 'module example.com/x\n' >"$d/go.mod"
expect "go.mod without a go directive fails" 1 "no 'go' directive in go.mod" "$d"

d=$(case_dir gr-drift)
edit 's#v2.17.1#v2.16.0#' "$d/Makefile"
expect "Makefile and release.yml goreleaser drift fails" 1 "the Makefile pins goreleaser v2.16.0 but .github/workflows/release.yml pins v2.17.1" "$d"

d=$(case_dir gr-split)
awk 'BEGIN{n=0} /version: v2.17.1/{n++; if(n==2) sub(/v2.17.1/, "v2.18.0")} {print}' \
  "$d/.github/workflows/release.yml" >"$d/r" && mv "$d/r" "$d/.github/workflows/release.yml"
expect "two goreleaser pins in release.yml that disagree fail" 1 "pins v2.17.1 v2.18.0" "$d"

d=$(case_dir gr-unmarked)
cat >>"$d/.github/workflows/release.yml" <<'EOF'
      - uses: goreleaser/goreleaser-action@f06c13b6b1a9625abc9e6e439d9c05a8f2190e94 # v7.2.3
        with:
          version: '~> v2'
EOF
expect "a goreleaser-action without the marked pin fails" 1 "3 goreleaser-action steps but 2 pinned" "$d"

d=$(case_dir gr-missing)
printf 'build:\n\tgo build ./...\n' >"$d/Makefile"
expect "missing Makefile pin fails" 1 "goreleaser pin not found" "$d"

d=$(case_dir syntax-floating)
{ printf '# syntax=docker/dockerfile:1\n'; cat "$d/Dockerfile.release"; } >"$d/x" && mv "$d/x" "$d/Dockerfile.release"
expect "a floating # syntax= frontend fails" 1 "Dockerfile.release pulls its Dockerfile frontend unpinned (# syntax=docker/dockerfile:1)" "$d"

d=$(case_dir syntax-pinned)
{ printf '# syntax=docker/dockerfile:1@sha256:%064d\n' 0; cat "$d/Dockerfile"; } >"$d/x" && mv "$d/x" "$d/Dockerfile"
expect "a digest-pinned # syntax= frontend passes" 0 "ok: Dockerfile golang:1.26.8" "$d"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "check-toolchain_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
