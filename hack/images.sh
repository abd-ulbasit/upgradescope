#!/usr/bin/env bash
# Builds every Dockerfile in the repository for every platform the release
# publishes, pushing nothing (make images; CI's images job). A PR that breaks
# an image build fails here instead of on the tag.
#
#   Dockerfile          the from-source image (web + Go stages, cross-compiled
#                       from the builder's platform, so no emulation)
#   Dockerfile.release  what the release publishes: it only COPYs a prebuilt
#                       binary, laid out the way GoReleaser stages its build
#                       context ($TARGETPLATFORM/upgradescope). The binaries
#                       are built here with the release's flags.
#
# Needs Docker with buildx and a builder that can build several platforms at
# once (docker-container driver, as docker/setup-buildx-action creates, or the
# containerd image store). Results stay in the build cache; nothing is loaded
# or pushed.
#
# Knobs:
#   IMAGES_PLATFORMS    comma-separated (default linux/amd64,linux/arm64)
#   IMAGES_DOCKERFILES  space-separated subset to build (default: all)
set -euo pipefail
cd "$(dirname "$0")/.."

PLATFORMS=${IMAGES_PLATFORMS:-linux/amd64,linux/arm64}
read -r -a dockerfiles <<<"${IMAGES_DOCKERFILES:-$(ls Dockerfile Dockerfile.* | tr '\n' ' ')}"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for df in "${dockerfiles[@]}"; do
  echo "== $df ($PLATFORMS)"
  case "$df" in
    Dockerfile.release)
      # Same build as .goreleaser.yml's: static, trimmed, stripped.
      IFS=, read -r -a plats <<<"$PLATFORMS"
      for p in "${plats[@]}"; do
        mkdir -p "$work/ctx/$p"
        CGO_ENABLED=0 GOOS="${p%/*}" GOARCH="${p#*/}" \
          go build -trimpath -ldflags "-s -w" -o "$work/ctx/$p/upgradescope" ./cmd/upgradescope
      done
      docker buildx build --platform "$PLATFORMS" -f "$df" "$work/ctx"
      ;;
    *)
      docker buildx build --platform "$PLATFORMS" -f "$df" .
      ;;
  esac
done
echo "images: OK"
