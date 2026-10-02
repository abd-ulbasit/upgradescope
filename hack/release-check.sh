#!/usr/bin/env bash
# Proves the packaging before a tag does (make release-check; CI's
# release-check job). Publishes and signs nothing:
#   1. `goreleaser check`: .goreleaser.yml is valid for the pinned GoReleaser;
#   2. `goreleaser release --snapshot`: every archive (and, with Docker, every
#      per-arch image from Dockerfile.release) builds;
#   3. the archive names are the ones the Action (action/run.sh) downloads from
#      each release — a drift here 404s every Action consumer;
#   4. the binary for this machine serves the embedded dashboard at / and
#      every asset it references (hack/dashboard-smoke.sh).
#
# GoReleaser runs through `go run` at GORELEASER_VERSION (the Makefile pins
# it; make check-toolchain keeps release.yml on the same version).
#
# Knobs:
#   GORELEASER_VERSION  required (the Makefile passes its pin)
#   GORELEASER_SKIP     --skip list for the snapshot (default publish,sign,sbom;
#                       add ,docker on a machine without a Docker engine)
set -euo pipefail
cd "$(dirname "$0")/.."

: "${GORELEASER_VERSION:?set GORELEASER_VERSION (make release-check passes the Makefile pin)}"
SKIP=${GORELEASER_SKIP:-publish,sign,sbom}
goreleaser() { go run "github.com/goreleaser/goreleaser/v2@$GORELEASER_VERSION" "$@"; }

die() { echo "::error::release-check: $*" >&2; exit 1; }
command -v jq >/dev/null || die "jq is required (brew install jq / apt install jq)"

echo "== goreleaser $GORELEASER_VERSION check"
goreleaser check

echo "== goreleaser $GORELEASER_VERSION release --snapshot --clean --skip=$SKIP"
goreleaser release --snapshot --clean --skip="$SKIP"

echo "== archive names match what action/run.sh downloads"
template="$(sed -n 's/^ *asset="\(.*\)"$/\1/p' action/run.sh)"
[ -n "$template" ] || die "no asset=\"...\" line in action/run.sh"
for os in linux darwin; do
  for arch in amd64 arm64; do
    name="${template//\$\{os\}/$os}"
    name="${name//\$\{arch\}/$arch}"
    [ -f "dist/$name" ] || { ls dist >&2; die "action/run.sh expects dist/$name, goreleaser did not produce it"; }
    echo "ok: $name"
  done
done
for arch in amd64 arm64; do
  [ -f "dist/upgradescope_windows_${arch}.zip" ] || die "no windows $arch zip"
  echo "ok: upgradescope_windows_${arch}.zip"
done

echo "== the release binary for this machine serves the dashboard and its assets"
goos=$(go env GOOS)
goarch=$(go env GOARCH)
bin=$(jq -r --arg os "$goos" --arg arch "$goarch" \
  '.[] | select(.type == "Binary" and .goos == $os and .goarch == $arch) | .path' dist/artifacts.json | head -1)
[ -n "$bin" ] && [ -x "$bin" ] || die "no $goos/$goarch binary in dist/artifacts.json"
"$bin" --version
hack/dashboard-smoke.sh "$bin"
echo "release-check: OK"
