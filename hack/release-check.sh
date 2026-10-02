#!/usr/bin/env bash
# Proves the release pipeline before a tag does (make release-check; CI's
# release-check job). Publishes and signs nothing:
#   1. `goreleaser check`: .goreleaser.yml is valid for the pinned GoReleaser;
#   2. `goreleaser release --snapshot`: every archive (and, with Docker, every
#      per-arch image from Dockerfile.release) builds;
#   3. the archive names are the ones the Action (action/run.sh) downloads from
#      each release — a drift here 404s every Action consumer;
#   4. the binary for this machine serves the embedded dashboard at /.
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

echo "== the release binary for this machine serves the dashboard at /"
goos=$(go env GOOS)
goarch=$(go env GOARCH)
bin=$(jq -r --arg os "$goos" --arg arch "$goarch" \
  '.[] | select(.type == "Binary" and .goos == $os and .goarch == $arch) | .path' dist/artifacts.json | head -1)
[ -n "$bin" ] && [ -x "$bin" ] || die "no $goos/$goarch binary in dist/artifacts.json"
"$bin" --version
db=$(mktemp -d)
"$bin" serve --listen 127.0.0.1:18080 --ingest-token smoke --db "$db/smoke.sqlite" &
pid=$!
# Also on failure: a live server would hold the job (or the terminal) open.
trap 'kill "$pid" 2>/dev/null || true; rm -rf "$db"' EXIT
for _ in $(seq 1 50); do curl -fsS -o /dev/null http://127.0.0.1:18080/healthz && break; sleep 0.2; done
got="$(curl -sS -o /dev/null -w '%{http_code} %{content_type}' http://127.0.0.1:18080/)"
case "$got" in
  '200 text/html'*) echo "ok: GET / -> $got" ;;
  *) die "GET / returned '$got', want 200 text/html (dashboard missing from the binary)" ;;
esac
echo "release-check: OK"
