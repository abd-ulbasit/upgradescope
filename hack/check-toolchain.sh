#!/usr/bin/env bash
# Asserts that what the repository builds with its own Go toolchain can be
# built with it (make check-toolchain; the repo-checks job runs it):
#
# 1. Every Dockerfile's golang base image matches go.mod's `go` directive. The
#    official golang images set GOTOOLCHAIN=local, so they will not fetch a
#    newer toolchain on demand: a base image older than the `go` directive
#    fails at `RUN go mod download` with
#
#      go: go.mod requires go >= X (running Y; GOTOOLCHAIN=local)
#
#    which breaks `make docker-build`, the images job and the kube job's
#    kind e2e. The from-source Dockerfile must have a golang stage;
#    Dockerfile.release packages prebuilt binaries and has none. No
#    Dockerfile may name an unpinned `# syntax=` frontend (#127): it is an
#    image pulled at build time, like the digest-pinned bases.
#
# 2. GoReleaser is pinned to one version: GORELEASER_VERSION in the Makefile
#    (make release-check, CI's release-check job) and every
#    goreleaser-action step in .github/workflows/release.yml. A release must
#    be built by the GoReleaser that CI proved the config with.
#
# 3. That version asks for no newer Go than go.mod does: `make release-check`
#    builds it with `go run`, and under GOTOOLCHAIN=local a goreleaser whose
#    go.mod needs a newer Go fails before it starts. This reads the pinned
#    version's go.mod through the module proxy, so it needs network access.
#
# 4. Every actions/setup-go step in .github/workflows reads go.mod
#    (`go-version-file: go.mod`), never a literal `go-version:`. A literal
#    like '1.26.x' lets setup-go take an older Go cached on the runner, which
#    then fails under GOTOOLCHAIN=local once go.mod asks for a newer patch
#    (kb-refresh-build.yml did, after go.mod moved to 1.26.9: #11).
#
# Knobs, for hack/check-toolchain_test.sh:
#   UPGRADESCOPE_TOOLCHAIN_ROOT     tree to check (default: this repository)
#   UPGRADESCOPE_TOOLCHAIN_OFFLINE  1 = skip check 3 (no module proxy)
set -euo pipefail
cd "${UPGRADESCOPE_TOOLCHAIN_ROOT:-$(dirname "$0")/..}"

want=$(awk '/^go [0-9]/ {print $2; exit}' go.mod)
[ -n "$want" ] || { echo "FAIL: no 'go' directive in go.mod" >&2; exit 1; }

rc=0
for df in Dockerfile*; do
  [ -f "$df" ] || continue
  # A `# syntax=` directive makes BuildKit pull that Dockerfile frontend
  # image before parsing; every base is digest-pinned, so it must be too.
  syntax=$(sed -nE '1,/^[^#]/ s/^#[[:space:]]*syntax[[:space:]]*=[[:space:]]*([^[:space:]]+).*/\1/p' "$df" | head -1)
  if [ -n "$syntax" ] && [[ "$syntax" != *@sha256:* ]]; then
    echo "FAIL: $df pulls its Dockerfile frontend unpinned (# syntax=$syntax);" >&2
    echo "      drop the line (the builder's frontend is used) or pin it by digest" >&2
    rc=1
  fi
  # The optional --platform flag is how the build stage cross-compiles from
  # the builder's platform for multi-arch images.
  got=$(sed -nE 's/^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?golang:([0-9][^-@ ]*).*/\2/p' "$df" | head -1)
  if [ -z "$got" ]; then
    if [ "$df" = Dockerfile ]; then
      echo "FAIL: Dockerfile has no 'FROM golang:<version>' line" >&2
      rc=1
    fi
  elif [ "$got" != "$want" ]; then
    echo "FAIL: $df builds on golang:$got but go.mod requires go $want" >&2
    echo "      (the golang image pins GOTOOLCHAIN=local, so it cannot upgrade itself)" >&2
    rc=1
  else
    echo "ok: $df golang:$got == go.mod go $want"
  fi
done

for f in .github/workflows/*.yml .github/workflows/*.yaml; do
  [ -f "$f" ] || continue
  steps=$(grep -c 'actions/setup-go@' "$f" || true)
  [ "$steps" -gt 0 ] || continue
  fromfile=$(grep -cE '^[[:space:]]+go-version-file:[[:space:]]*go\.mod([[:space:]]|$)' "$f" || true)
  literal=$(grep -nE '^[[:space:]]+go-version:' "$f" || true)
  if [ -n "$literal" ]; then
    echo "FAIL: $f pins Go literally instead of reading go.mod:" >&2
    sed 's/^/      /' <<<"$literal" >&2
    echo "      use 'go-version-file: go.mod' (a literal can resolve to an older cached Go)" >&2
    rc=1
  elif [ "$steps" != "$fromfile" ]; then
    echo "FAIL: $f has $steps setup-go steps but $fromfile read 'go-version-file: go.mod'" >&2
    rc=1
  else
    echo "ok: $f: $steps setup-go step(s) read go.mod"
  fi
done

# newer A B: Go version A is strictly newer than B.
newer() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" = "$1" ]; }

wf=.github/workflows/release.yml
gr=$(sed -nE 's/^GORELEASER_VERSION[[:space:]]*\?=[[:space:]]*(v[^[:space:]]+).*/\1/p' Makefile | head -1)
# Every goreleaser-action step carries `version: vX.Y.Z # ... GORELEASER_VERSION ...`.
steps=$(grep -c 'goreleaser/goreleaser-action@' "$wf" || true)
pins=$(sed -nE 's/^[[:space:]]+version:[[:space:]]*(v[^[:space:]]+).*GORELEASER_VERSION.*/\1/p' "$wf")
npins=$(grep -c . <<<"$pins" || true)
distinct=$(sort -u <<<"$pins" | tr '\n' ' ' | sed 's/ $//')
if [ -z "$gr" ] || [ -z "$pins" ]; then
  echo "FAIL: goreleaser pin not found (Makefile GORELEASER_VERSION: '$gr'; $wf version: '$distinct')" >&2
  rc=1
elif [ "$steps" != "$npins" ]; then
  echo "FAIL: $wf has $steps goreleaser-action steps but $npins pinned with a" >&2
  echo "      'version: vX.Y.Z # ... GORELEASER_VERSION' line; pin every one" >&2
  rc=1
elif [ "$distinct" != "$gr" ]; then
  if [ "$(sort -u <<<"$pins" | wc -l)" -gt 1 ]; then
    echo "FAIL: $wf pins $distinct; every goreleaser-action step must pin the Makefile's $gr" >&2
  else
    echo "FAIL: the Makefile pins goreleaser $gr but $wf pins $distinct" >&2
  fi
  rc=1
elif [ "${UPGRADESCOPE_TOOLCHAIN_OFFLINE:-}" = 1 ]; then
  echo "ok: goreleaser $gr (Makefile, $wf); its Go requirement not checked (offline)"
else
  gomod=$(go list -m -f '{{.GoMod}}' "github.com/goreleaser/goreleaser/v2@$gr")
  gr_go=$(awk '/^go [0-9]/ {print $2; exit}' "$gomod")
  if [ -z "$gr_go" ]; then
    echo "FAIL: goreleaser $gr: no 'go' directive in $gomod" >&2
    rc=1
  elif newer "$gr_go" "$want"; then
    echo "FAIL: goreleaser $gr requires go >= $gr_go, newer than go.mod's go $want," >&2
    echo "      so 'make release-check' cannot build it under GOTOOLCHAIN=local;" >&2
    echo "      pin a goreleaser whose go.mod needs go $want or older" >&2
    rc=1
  else
    echo "ok: goreleaser $gr (Makefile, $wf) needs go $gr_go <= go.mod go $want"
  fi
fi
exit $rc
