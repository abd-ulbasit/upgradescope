#!/usr/bin/env bash
# Tests for hack/flags-diff.sh (make hack-test): a flag or command the new
# build lost, a changed default or type, and a changed usage line each need a
# mention in CHANGELOG.md's Changed sections since the base release. Offline:
# the two binaries are stubs that print canned cobra help.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"

# stub <dir>: a "binary" printing <dir>/help/<root_cmd_sub> for `<cmd> <sub> --help`.
stub() {
  mkdir -p "$1/help"
  cat >"$1/bin" <<'STUB'
#!/usr/bin/env bash
args=("$@")
unset 'args[${#args[@]}-1]'
name=root
for a in ${args[@]+"${args[@]}"}; do name="${name}_$a"; done
cat "$(dirname "$0")/help/$name"
STUB
  chmod +x "$1/bin"
}

# root <dir> <subcommands...>
root() {
  local d=$1; shift
  {
    printf 'Does things.\n\nUsage:\n  upgradescope [command]\n\nAvailable Commands:\n'
    for c in "$@"; do printf '  %-11s does %s\n' "$c" "$c"; done
    printf '  completion  Generate the autocompletion script\n  help        Help about any command\n\nFlags:\n  -h, --help   help for upgradescope\n'
  } >"$d/help/root"
}
# leaf <dir> <name> <usage> <flag lines...>: a command with no children.
leaf() {
  local d=$1 name=$2 usage=$3; shift 3
  {
    printf 'Long text about %s.\n\nUsage:\n  upgradescope %s\n\nFlags:\n' "$name" "$usage"
    for f in "$@"; do printf '%s\n' "$f"; done
    printf '  -h, --help   help for %s\n' "$name"
  } >"$d/help/root_$name"
}

listen_old='      --listen string   address to listen on (default ":8080")'
retention='      --retention string   prune older than this (default "90d")'
base() { # base <dir>: the "release" build
  stub "$1"; root "$1" scan serve
  leaf "$1" scan 'scan [flags]' '      --target string   Kubernetes version'
  leaf "$1" serve 'serve [flags]' "$listen_old" "$retention"
}

changelog() { # changelog <file> <unreleased Changed entry> <unreleased Added entry>
  {
    printf '# Changelog\n\n## [Unreleased]\n\n'
    [ -n "$3" ] && printf '### Added\n\n- %s\n\n' "$3"
    [ -n "$2" ] && printf '### Changed\n\n- %s\n\n' "$2"
    printf '## [0.1.1] - 2026-07-27\n\n### Changed\n\n- Old `--target` wording.\n'
  } >"$1"
}

expect() { # expect <name> <want-exit> <substring> <changelog> <old> <new>
  local name=$1 want=$2 needle=$3 cl=$4 old=$5 new=$6 got=0
  hack/flags-diff.sh --old-bin "$old/bin" --new-bin "$new/bin" --changelog "$cl" v0.1.1 >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

old="$work/old"; base "$old"
cl="$work/none.md"; changelog "$cl" "" ""

same="$work/same"; base "$same"
expect "identical builds pass" 0 "no flag, default or command" "$cl" "$old" "$same"

added="$work/added"; base "$added"
leaf "$added" serve 'serve [flags]' "$listen_old" "$retention" '      --new-flag string   brand new'
expect "an added flag passes" 0 "no flag, default or command" "$cl" "$old" "$added"

removed="$work/removed"; base "$removed"
leaf "$removed" serve 'serve [flags]' "$listen_old"
expect "a removed flag with no mention fails" 1 "serve: flag --retention was removed" "$cl" "$old" "$removed"

c="$work/mentioned.md"; changelog "$c" 'The `--retention` flag of `serve` is gone; use the chart value.' ""
expect "a removed flag named under Changed passes" 0 "all mentioned" "$c" "$old" "$removed"

c="$work/added-only.md"; changelog "$c" "" 'Mentions `--retention` but only under Added.'
expect "a mention under Added does not count" 1 "--retention" "$c" "$old" "$removed"

c="$work/prefix.md"; changelog "$c" 'The `--retention-days` flag changed.' ""
expect "a longer flag name is not a mention" 1 "--retention" "$c" "$old" "$removed"

dflt="$work/default"; base "$dflt"
leaf "$dflt" serve 'serve [flags]' '      --listen string   address to listen on (default "127.0.0.1:8080")' "$retention"
expect "a changed default with no mention fails" 1 'default ":8080" -> "127.0.0.1:8080"' "$cl" "$old" "$dflt"
c="$work/dflt.md"; changelog "$c" 'serve `--listen` now defaults to 127.0.0.1:8080; pass `--listen :8080`.' ""
expect "a changed default named under Changed passes" 0 "all mentioned" "$c" "$old" "$dflt"

c="$work/dflt-name.md"; changelog "$c" 'serve `--listen` refuses open read access off loopback.' ""
expect "naming the flag without its new default is not enough" 1 "'--listen' and '127.0.0.1:8080'" "$c" "$old" "$dflt"

typed="$work/typed"; base "$typed"
leaf "$typed" serve 'serve [flags]' '      --listen int   address to listen on (default ":8080")' "$retention"
expect "a changed type fails" 1 "type string -> int" "$cl" "$old" "$typed"

gone="$work/gone"; stub "$gone"; root "$gone" scan
leaf "$gone" scan 'scan [flags]' '      --target string   Kubernetes version'
expect "a removed command fails" 1 "command serve was removed" "$cl" "$old" "$gone"
if grep -qF -- "--retention" "$work/out"; then
  echo "FAIL a removed command's flags are not listed" >&2; echo "FAIL removed command flags" >>"$work/results"
else
  echo "ok   a removed command's flags are not listed" | tee -a "$work/results"
fi

usage="$work/usage"; base "$usage"
leaf "$usage" scan 'scan <dir> [flags]' '      --target string   Kubernetes version'
expect "a changed usage line fails" 1 "scan: usage changed" "$cl" "$old" "$usage"
c="$work/usage.md"; changelog "$c" '`scan` takes a directory argument.' ""
expect "a changed usage line named under Changed passes" 0 "all mentioned" "$c" "$old" "$usage"

c="$work/release.md"
{
  printf '# Changelog\n\n## [Unreleased]\n\n## [0.2.0] - 2026-10-20\n\n### Changed\n\n- Dropped `--retention`.\n\n'
  printf '## [0.1.1] - 2026-07-27\n\n### Added\n\n- Old.\n'
} >"$c"
expect "a release PR's moved section still counts" 0 "all mentioned" "$c" "$old" "$removed"

c="$work/oldchange.md"
printf '# Changelog\n\n## [Unreleased]\n\n## [0.1.1] - 2026-07-27\n\n### Changed\n\n- Dropped `--retention`.\n' >"$c"
expect "a mention in the base release's own section does not count" 1 "--retention" "$c" "$old" "$removed"

c="$work/nobase.md"
printf '# Changelog\n\n## [Unreleased]\n\n### Changed\n\n- `--retention`.\n' >"$c"
expect "a changelog without the base release's section is refused" 1 "## [0.1.1]" "$c" "$old" "$removed"

expect "a missing changelog is refused" 1 "no $work/absent.md" "$work/absent.md" "$old" "$same"

broken="$work/broken"; mkdir -p "$broken"
printf '#!/usr/bin/env bash\nexit 3\n' >"$broken/bin"; chmod +x "$broken/bin"
expect "a binary whose --help fails is an error, not a pass" 1 "failed" "$cl" "$broken" "$same"

echo
if grep -q '^FAIL' "$work/results"; then
  echo "$(grep -c '^FAIL' "$work/results") failed" >&2
  exit 1
fi
echo "$(grep -c '^ok' "$work/results") passed"
