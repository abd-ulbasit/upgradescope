#!/usr/bin/env bash
# The kube job's Kubernetes version matrix, from hack/kind-node-images.txt:
#
#   hack/kind-images.sh matrix pr|all   JSON array of minors, e.g. ["1.31","1.37"]
#   hack/kind-images.sh image <minor>   the digest-pinned kind node image
#   hack/kind-images.sh next <minor>    the next minor (the default upgrade target)
#   hack/kind-images.sh set <event> <release> <full-matrix>
#                                       the matrix a ci.yml run takes: all on the
#                                       schedule and on workflow_dispatch (unless
#                                       full-matrix=false), pr otherwise and on
#                                       every release run
#
# The table is validated on every call, so a hand edit that drops a digest
# or mislabels a row fails the job that reads it.
# Knob: KIND_IMAGES_TABLE (default hack/kind-node-images.txt).
set -euo pipefail
cd "$(dirname "$0")/.."
TABLE=${KIND_IMAGES_TABLE:-hack/kind-node-images.txt}

usage() { echo "usage: $0 matrix pr|all | image <minor> | next <minor> | set <event> <release> <full-matrix>" >&2; exit 2; }
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
  set\ *)
    # A release (release.yml's workflow_call, release: true) carries the
    # caller's event, so a re-dispatched release looks like a dispatch here;
    # it keeps the PR set, since older kind images may stop booting.
    [ $# -eq 4 ] || usage
    case "$4" in true | false | '') ;; *) echo "kind-images: full-matrix must be true or false, not '$4'" >&2; exit 2 ;; esac
    case "$3/$2/$4" in
      true/*) echo pr ;;
      */schedule/*) echo all ;;
      */workflow_dispatch/false) echo pr ;;
      */workflow_dispatch/*) echo all ;;
      *) echo pr ;;
    esac
    ;;
  next\ 1.*)
    [[ "$2" =~ ^1\.([0-9]+)$ ]] || usage
    echo "1.$((BASH_REMATCH[1] + 1))"
    ;;
  *) usage ;;
esac
