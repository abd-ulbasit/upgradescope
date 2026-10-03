#!/usr/bin/env bash
# Reproducibility of the release (make release-repro; #127): builds the
# GoReleaser snapshot of HEAD twice, each from its own fresh clone (new
# paths, new file mtimes), and asserts the two produce identical
# checksums.txt (every archive, deb/rpm/apk package and api/ contract) and, when images are
# built, identical per-platform image IDs. A difference means something
# from the build machine or the clock leaked into an artifact, and the
# published checksums could not be reproduced from the tag.
#
# Two builds on one machine cannot see that machine's own name (the rpm's
# Build Host header was the build host's name until it was pinned, #198), so
# the packages are also scanned for this host's name and the clones' paths
# (hack/check-host-leak.sh). The two clones differ in path; the host name
# cannot be changed without privileges, so the scan stands in for varying it.
#
# Knobs:
#   GORELEASER_VERSION  required (the Makefile passes its pin)
#   GORELEASER_SKIP     --skip list (default publish,sign,sbom,docker; drop
#                       docker to compare images too, which needs a Docker
#                       engine with buildx)
set -euo pipefail
cd "$(dirname "$0")/.."

: "${GORELEASER_VERSION:?set GORELEASER_VERSION (make release-repro passes the Makefile pin)}"
SKIP=${GORELEASER_SKIP:-publish,sign,sbom,docker}
die() { echo "::error::release-repro: $*" >&2; exit 1; }
command -v jq >/dev/null || die "jq is required (brew install jq / apt install jq)"
git diff --quiet HEAD || echo "note: uncommitted changes are not part of the clones; this checks HEAD" >&2

images=1
case ",$SKIP," in *,docker,*) images=0 ;; esac
# A snapshot --loads one image per platform; its ID is the config digest,
# which covers the layers and the created time. Read right after each run:
# the second run re-tags the same names.
ids() { jq -r '.[] | select(.type == "Docker Image") | .name' "$1/dist/artifacts.json" | sort -u |
  while read -r img; do echo "$img $(docker image inspect --format '{{ .Id }}' "$img")"; done; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
src="$(pwd)"
for run in a b; do
  echo "== run $run: fresh clone of HEAD, goreleaser $GORELEASER_VERSION snapshot (--skip=$SKIP)"
  git clone --quiet --no-local "$src" "$work/$run"
  git -C "$work/$run" checkout --quiet --detach "$(git rev-parse HEAD)"
  # The clones get every tag, so the snapshot version is the same in both.
  (cd "$work/$run" && go run "github.com/goreleaser/goreleaser/v2@$GORELEASER_VERSION" \
    release --snapshot --clean --skip="$SKIP" >"$work/$run.log" 2>&1) \
    || { tail -40 "$work/$run.log" >&2; die "snapshot $run failed"; }
  if [ "$images" = 1 ]; then
    ids "$work/$run" >"$work/$run.ids"
    [ -s "$work/$run.ids" ] || die "no images in run $run"
  fi
  sleep 1 # a second apart, so a build-clock timestamp would differ
done

if ! diff -u "$work/a/dist/checksums.txt" "$work/b/dist/checksums.txt"; then
  die "two builds of one commit produced different archives or packages (diff above)"
fi
echo "ok: checksums.txt identical ($(grep -c . "$work/a/dist/checksums.txt") artifacts)"

# This host's name and the two build paths (also resolved: macOS's /var is a
# link into /private) must not be in any package.
leaks="$work/a
$work/b
$(cd "$work/a" && pwd -P)
$(cd "$work/b" && pwd -P)"
for run in a b; do
  pkgs=()
  while IFS= read -r p; do pkgs+=("$work/$run/$p"); done \
    < <(jq -r '.[] | select(.type == "Linux Package") | .path' "$work/$run/dist/artifacts.json")
  [ "${#pkgs[@]}" -gt 0 ] || die "no Linux Package in run $run"
  UPGRADESCOPE_LEAK_NAMES=$leaks hack/check-host-leak.sh "${pkgs[@]}" \
    || die "a package from run $run carries a build-machine name (above)"
done

if [ "$images" = 1 ]; then
  diff -u "$work/a.ids" "$work/b.ids" || die "per-platform images differ between the two builds"
  echo "ok: per-platform image IDs identical"
else
  echo "images not compared (GORELEASER_SKIP has docker)"
fi
echo "release-repro: OK"
