#!/usr/bin/env bash
# The kube job's Kubernetes version matrix, from hack/kind-node-images.txt:
#
#   hack/kind-images.sh matrix pr|all   JSON array of minors, e.g. ["1.31","1.37"]
#   hack/kind-images.sh image <minor>   the digest-pinned kind node image
#   hack/kind-images.sh next <minor>    the next minor (the default upgrade target)
#
# The table is validated on every call, so a hand edit that drops a digest
# or mislabels a row fails the job that reads it.
# Knob: KIND_IMAGES_TABLE (default hack/kind-node-images.txt).
set -euo pipefail
cd "$(dirname "$0")/.."
TABLE=${KIND_IMAGES_TABLE:-hack/kind-node-images.txt}

usage() { echo "usage: $0 matrix pr|all | image <minor> | next <minor>" >&2; exit 2; }
bad() { echo "kind-images: $TABLE: $*" >&2; exit 2; }

rows=()
while read -r minor image when _; do
  case "$minor" in '' | '#'*) continue ;; esac
  [[ "$minor" =~ ^1\.[0-9]+$ ]] || bad "bad minor '$minor'"
  [[ "$image" =~ ^kindest/node:v[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}$ ]] ||
    bad "$minor: '$image' is not pinned by digest (kindest/node:vX.Y.Z@sha256:...)"
  [[ "$image" == "kindest/node:v$minor."* ]] || bad "$minor: '$image' is not a $minor image"
  case "$when" in pr | weekly) ;; *) bad "$minor: schedule '$when' must be pr or weekly" ;; esac
  rows+=("$minor $image $when")
done <"$TABLE"

case "${1:-} ${2:-}" in
  "matrix pr" | "matrix all")
    for r in "${rows[@]}"; do
      read -r minor _ when <<<"$r"
      if [ "$2" = all ] || [ "$when" = pr ]; then echo "$minor"; fi
    done | jq -R . | jq -cs .
    ;;
  image\ 1.*)
    for r in "${rows[@]}"; do
      read -r minor image _ <<<"$r"
      if [ "$minor" = "$2" ]; then echo "$image"; exit 0; fi
    done
    echo "kind-images: no kind node image for $2 in $TABLE" >&2
    exit 1
    ;;
  next\ 1.*)
    [[ "$2" =~ ^1\.([0-9]+)$ ]] || usage
    echo "1.$((BASH_REMATCH[1] + 1))"
    ;;
  *) usage ;;
esac
