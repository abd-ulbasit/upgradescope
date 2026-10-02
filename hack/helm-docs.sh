#!/usr/bin/env bash
# Renders the chart's values reference with helm-docs (pinned and
# checksum-verified by hack/install-tool.sh) from the `# --` comments in
# deploy/chart/values.yaml, into two files:
#   deploy/chart/README.md            from hack/docs/chart-README.md.gotmpl
#   docs/reference/helm-values.md     from hack/docs/helm-values.md.gotmpl
# Both use the escaping table in hack/docs/values-table.gotmpl.
#
#   hack/helm-docs.sh           rewrite both files
#   hack/helm-docs.sh --check   fail when either is stale (make docs-check, CI's docs workflow)
set -euo pipefail
cd "$(dirname "$0")/.."

check=false
case "${1:-}" in
  "") ;;
  --check) check=true ;;
  *) echo "usage: $0 [--check]" >&2; exit 2 ;;
esac

helm_docs=$(hack/install-tool.sh helm-docs)
render() { # render <template> <output, relative to the chart>
  "$helm_docs" --log-level warning --chart-search-root deploy/chart \
    --template-files ../../hack/docs/values-table.gotmpl \
    --template-files "../../hack/docs/$1" \
    --output-file "$2"
}

if $check; then
  # Render into a scratch copy of the repository files and compare, so a
  # check never rewrites the working tree.
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  cp deploy/chart/README.md "$work/README.md"
  cp docs/reference/helm-values.md "$work/helm-values.md"
fi

render chart-README.md.gotmpl README.md
render helm-values.md.gotmpl ../../docs/reference/helm-values.md

if $check; then
  stale=0
  for pair in "deploy/chart/README.md $work/README.md" "docs/reference/helm-values.md $work/helm-values.md"; do
    set -- $pair
    if ! cmp -s "$1" "$2"; then
      echo "::error file=$1::$1 is stale: run make helm-docs and commit the result" >&2
      diff -u "$2" "$1" >&2 || true
      cp "$2" "$1" # leave the tree as it was
      stale=1
    fi
  done
  exit "$stale"
fi
