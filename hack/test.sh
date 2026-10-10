#!/usr/bin/env bash
# The Go unit gate (make test; CI's test job), for the main module and the
# separate tools/ modules:
#   - go vet;
#   - gofmt over every tracked Go file, tools/ included;
#   - go test -race -count=1. The agent loop, the server's ingest/gate caps
#     and the notifier are concurrent; -count=1 so a cached pass never stands
#     in for a run. Integration suites stay env-gated (UPGRADESCOPE_IT,
#     UPGRADESCOPE_PG_TEST_DSN), so this needs no cluster, database or Docker.
#     The heap-bound tests skip under -race; make test-heap runs them.
#
# Two ways to run less of it, both for CI (a laptop runs it all):
#
#   --shard <i>/<n>   one of n balanced groups of packages. Every package of
#                     every module is a unit, assigned to exactly one shard by
#                     hack/shard.sh, greedy longest-first from the committed
#                     hack/test-durations.txt (a package with no entry counts
#                     10 s, so a new package is placed without editing
#                     anything). CI runs the shards as the test job's matrix:
#                     one runner took 9.4 minutes for the main module alone.
#   --docs-readers    only the packages whose tests read documentation
#                     (hack/test-docs-readers.txt). For a pull request that
#                     changes documentation and no code: the docs-drift tests
#                     (the CLI's flags in docs/, the metrics reference, the
#                     chart's upgrade notes) fail on a stale page, so a
#                     docs-only change cannot skip them, but it needs no more.
#
# --list prints the "<module dir> <package dir>" units the other flags
# select and runs nothing.
set -euo pipefail
cd "$(dirname "$0")/.."

usage() {
  echo "usage: hack/test.sh [--list] [--shard <i>/<n>] [--docs-readers]" >&2
  exit 2
}
list=false
shard=""
docs=false
while [ $# -gt 0 ]; do
  case "$1" in
    --list) list=true; shift ;;
    --shard) [ $# -ge 2 ] || usage; shard=$2; shift 2 ;;
    --docs-readers) docs=true; shift ;;
    *) usage ;;
  esac
done

# "<module dir> <package dir relative to it>" for every package of the main
# module (module dir ".") and of each tools/ module, "." being a module's
# root package. go list -find resolves no imports, so it is quick and needs
# no module download beyond the module graph.
units() {
  local mod mp
  for mod in . tools/*/; do
    mod=${mod%/}
    [ "$mod" = . ] || [ -f "$mod/go.mod" ] || continue
    mp=$(cd "$mod" && go list -m -f '{{.Path}}')
    (cd "$mod" && go list -find -f '{{.ImportPath}}' ./...) |
      awk -v mod="$mod" -v mp="$mp" '
        { if ($0 == mp) d = "."; else if (index($0, mp "/") == 1) d = substr($0, length(mp) + 2); else d = $0
          print mod, d }'
  done | LC_ALL=C sort -u
}

all_units=$(units)
selected=$all_units
if [ -z "$selected" ]; then
  echo "test: go list found no packages" >&2
  exit 1
fi
if $docs; then
  readers=$(sed -e 's/#.*//' -e '/^[[:space:]]*$/d' hack/test-docs-readers.txt)
  selected=$(LC_ALL=C comm -12 <(LC_ALL=C sort <<<"$selected") <(LC_ALL=C sort <<<"$readers"))
  if [ -z "$selected" ]; then
    echo "test: hack/test-docs-readers.txt names no package of this tree" >&2
    exit 1
  fi
fi
if [ -n "$shard" ]; then
  selected=$(hack/shard.sh "$shard" -d hack/test-durations.txt -c 10 <<<"$selected")
fi
if $list; then
  [ -z "$selected" ] || echo "$selected"
  exit 0
fi

echo "== gofmt"
unformatted="$(git ls-files -z '*.go' | xargs -0 gofmt -l)"
if [ -n "$unformatted" ]; then
  echo "::error::gofmt needed on:" >&2
  echo "$unformatted" >&2
  exit 1
fi

if [ -z "$selected" ]; then
  echo "test: OK (shard $shard has no packages: fewer packages than shards)"
  exit 0
fi

# Run one module's selected packages. All of them: ./... , the old command.
run_module() { # <module dir> <package dirs...>
  local mod=$1 all args=() p
  shift
  all=$(awk -v m="$mod" '$1 == m' <<<"$all_units" | wc -l | tr -d ' ')
  if [ "$#" -eq "$all" ]; then
    args=("./...")
  else
    for p in "$@"; do args+=("./$p"); done
  fi
  echo "== go vet + go test -race ($mod, $# of $all packages)"
  (cd "$mod" && go vet "${args[@]}" && go test -race -count=1 "${args[@]}")
}

for mod in $(cut -d' ' -f1 <<<"$selected" | LC_ALL=C sort -u); do
  # shellcheck disable=SC2046
  run_module "$mod" $(awk -v m="$mod" '$1 == m { print $2 }' <<<"$selected")
done
echo "test: OK${shard:+ (shard $shard)}"
