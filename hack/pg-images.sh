#!/usr/bin/env bash
# The pg-conformance job's Postgres matrix, from hack/pg-images.txt:
#
#   hack/pg-images.sh matrix pr|all   JSON array of {"pg": <major>, "image":
#                                     <digest-pinned image>}, in table order
#
# Which set a run takes is hack/kind-images.sh set's answer (the same rule
# as the kube job: all on the schedule and workflow_dispatch unless
# full-matrix=false, pr otherwise and on every release run).
# The table is validated on every call, so a hand edit that drops a digest
# or mislabels a row fails the job that reads it.
# Knob: PG_IMAGES_TABLE (default hack/pg-images.txt).
set -euo pipefail
cd "$(dirname "$0")/.."
TABLE=${PG_IMAGES_TABLE:-hack/pg-images.txt}

usage() { echo "usage: $0 matrix pr|all" >&2; exit 2; }
bad() { echo "pg-images: $TABLE: $*" >&2; exit 2; }

rows=()
while read -r major image when extra; do
  case "$major" in '' | '#'*) continue ;; esac
  [[ "$major" =~ ^[0-9]+$ ]] || bad "bad major '$major'"
  [[ "$image" =~ ^postgres:$major@sha256:[0-9a-f]{64}$ ]] ||
    bad "$major: '$image' is not postgres:$major pinned by digest (postgres:$major@sha256:...)"
  case "$when" in pr | weekly) ;; *) bad "$major: schedule '$when' must be pr or weekly" ;; esac
  [ -z "$extra" ] || bad "$major: unexpected '$extra' after the schedule"
  rows+=("$major $image $when")
done <"$TABLE"
[ "${#rows[@]}" -gt 0 ] || bad "no images"

case "${1:-} ${2:-}" in
  "matrix pr" | "matrix all")
    [ $# -eq 2 ] || usage
    for r in "${rows[@]}"; do
      read -r major image when <<<"$r"
      if [ "$2" = all ] || [ "$when" = pr ]; then jq -nc --arg pg "$major" --arg image "$image" '{pg: $pg, image: $image}'; fi
    done | jq -cs .
    ;;
  *) usage ;;
esac
