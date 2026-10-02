#!/usr/bin/env bash
# Every user-visible CLI break since the last release must be in CHANGELOG.md
# (#167). v0.1.1 -> main changed serve --listen's default, removed `tokens
# revoke <cluster>` and rewrote the webhook body, and the changelog said none
# of it: compatibility-policy.md promises that a flag change is listed under
# Changed. This builds the last release tag and HEAD, walks both command
# trees through `--help`, and fails when the new build
#   - lost a flag or a command,
#   - changed a flag's default or type, or
#   - changed a command's usage line (arguments, required flag groups)
# without the changelog naming it: `--flag` for a flag, the command path
# ("tokens revoke") for a command or a usage line. Names are matched in the
# ### Changed lists of every section above the base release's, so a release PR
# that has moved [Unreleased] into "## [X.Y.Z]" still passes. A new flag, a
# relaxed requirement or a reworded description is not checked: only the
# contract a script written against the old build can trip over is.
#
# Usage: hack/flags-diff.sh [--old-bin PATH] [--new-bin PATH] [--changelog FILE] [<base-tag>]
#   <base-tag>   the release to compare with (default: the newest stable vX.Y.Z
#                tag before HEAD's first parent, so a tagged HEAD compares with
#                the release before it)
#   --old-bin    a built binary of <base-tag> (default: built from `git archive`)
#   --new-bin    a built binary of this tree (default: `go build ./cmd/upgradescope`)
# Needs the tags (CI checks out with fetch-depth 0). hack/flags-diff_test.sh
# drives it with stub binaries; make flags-diff runs it on this tree.
set -euo pipefail
cd "$(dirname "$0")/.."

old_bin= new_bin= changelog=CHANGELOG.md tag=
while [ $# -gt 0 ]; do
  case $1 in
    --old-bin) old_bin=${2:?--old-bin needs a path}; shift 2 ;;
    --new-bin) new_bin=${2:?--new-bin needs a path}; shift 2 ;;
    --changelog) changelog=${2:?--changelog needs a file}; shift 2 ;;
    -*) echo "usage: $0 [--old-bin PATH] [--new-bin PATH] [--changelog FILE] [<base-tag>]" >&2; exit 2 ;;
    *) tag=$1; shift ;;
  esac
done
die() { echo "::error file=$changelog::flags-diff: $*" >&2; exit 1; }

[ -f "$changelog" ] || die "no $changelog"
if [ -z "$tag" ]; then
  tag=$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' --exclude '*-*' 'HEAD^' 2>/dev/null) ||
    die "no release tag before HEAD (checkout with fetch-depth: 0), or pass <base-tag>"
fi
printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || die "'$tag' is not a vX.Y.Z tag"
version=${tag#v}
grep -qF "## [$version]" "$changelog" ||
  die "$changelog has no '## [$version]' section, so it cannot tell which sections are newer than $tag"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if [ -z "$old_bin" ]; then
  mkdir "$work/rel"
  git archive "$tag" | tar -x -C "$work/rel"
  (cd "$work/rel" && go build -o "$work/old-bin" ./cmd/upgradescope) || die "could not build $tag"
  old_bin=$work/old-bin
fi
if [ -z "$new_bin" ]; then
  go build -o "$work/new-bin" ./cmd/upgradescope || die "could not build this tree"
  new_bin=$work/new-bin
fi

# parse_help: one cobra help page on stdin -> TSV on stdout,
#   CMD  <path> <usage lines joined with " | ">
#   FLAG <path> --name <type> <default>
# Flag descriptions wrap onto indented lines; pflag puts "(default X)" last.
# Set path to "-" for the root.
parse_help() {
  awk -v path="$1" '
    function flush(   spec, i, name, type, def, rest) {
      if (rec == "") return
      i = index(rec, "  ")
      spec = i ? substr(rec, 1, i - 1) : rec
      i = index(spec, " ")
      name = i ? substr(spec, 1, i - 1) : spec
      type = i ? substr(spec, i + 1) : ""
      def = ""; rest = rec
      while (match(rest, /\(default [^)]*\)/)) {
        def = substr(rest, RSTART + 9, RLENGTH - 10)
        rest = substr(rest, RSTART + RLENGTH)
      }
      printf "FLAG\t%s\t%s\t%s\t%s\n", path, name, type, def
      rec = ""
    }
    /^Usage:/ { u = 1; next }
    u && /^$/ { u = 0; printf "CMD\t%s\t%s\n", path, usage; next }
    u { sub(/^ +/, ""); usage = usage (usage == "" ? "" : " | ") $0; next }
    /^(Global )?Flags:/ { f = 1; next }
    f && /^$/ { flush(); f = 0; next }
    f && /^ +(-[A-Za-z], )?--[A-Za-z0-9]/ { flush(); rec = $0; sub(/^ +(-[A-Za-z], )?/, "", rec); next }
    f { t = $0; sub(/^ +/, "", t); rec = rec " " t }
    END { flush() }
  '
}

# dump <bin> <path words...>: the command and every subcommand below it.
dump() {
  local bin=$1; shift
  local help
  help=$("$bin" "$@" --help 2>&1) || die "'$(basename "$bin") $* --help' failed: $help"
  parse_help "${*:--}" <<<"$help"
  local sub
  while read -r sub; do
    case $sub in '' | help | completion) continue ;; esac
    dump "$bin" "$@" "$sub"
  done < <(awk '/^Available Commands:/ { on = 1; next } on && /^$/ { exit } on { print $1 }' <<<"$help")
}

dump "$old_bin" | sort >"$work/old.tsv"
dump "$new_bin" | sort >"$work/new.tsv"
[ -s "$work/old.tsv" ] && [ -s "$work/new.tsv" ] || die "a build printed no commands"

# problems: <kind> TAB <word the changelog must contain> TAB <a second word it
# must contain, or "-"> TAB <message>. A changed default also needs the new
# value: a note that names --listen for some other change is no migration note
# for its default.
awk -F'\t' '
  FNR == NR {
    if ($1 == "CMD") { oc[$2] = $3 } else { of[$2 SUBSEP $3] = $4 "\t" $5 }
    next
  }
  $1 == "CMD" { nc[$2] = $3; next }
  { nf[$2 SUBSEP $3] = $4 "\t" $5 }
  END {
    for (p in oc) {
      w = (p == "-") ? "upgradescope" : p
      if (!(p in nc)) { printf "command\t%s\t-\tcommand %s was removed\n", w, p; continue }
      if (oc[p] != nc[p]) printf "command\t%s\t-\t%s: usage changed: %s -> %s\n", w, p, oc[p], nc[p]
    }
    for (k in of) {
      split(k, a, SUBSEP)
      if (!(a[1] in nc)) continue        # the whole command is already reported
      if (!(k in nf)) { printf "flag\t%s\t-\t%s: flag %s was removed\n", a[2], a[1], a[2]; continue }
      if (of[k] != nf[k]) {
        split(of[k], o, "\t"); split(nf[k], n, "\t")
        m = a[1] ": flag " a[2] " changed:"
        if (o[1] != n[1]) m = m " type " o[1] " -> " n[1] ";"
        if (o[2] != n[2]) m = m " default " o[2] " -> " n[2]
        d = n[2]; gsub(/^"|"$/, "", d)
        printf "flag\t%s\t%s\t%s\n", a[2], (o[2] != n[2] && d != "" ? d : "-"), m
      }
    }
  }
' "$work/old.tsv" "$work/new.tsv" | sort >"$work/problems.tsv"

# The Changed lists of every section above ## [<base version>].
awk -v stop="## [$version]" '
  index($0, stop) == 1 { exit }
  /^## / { on = 0 }
  /^### / { on = ($0 ~ /^### Changed/) }
  on { print }
' "$changelog" >"$work/changed.txt"

missing=0
while IFS=$'\t' read -r kind word value msg; do
  [ -n "$kind" ] || continue
  found=1
  if [ "$kind" = flag ]; then
    # the flag name, not a longer one that starts with it
    grep -Eq -- "$word([^A-Za-z0-9-]|\$)" "$work/changed.txt" || found=0
  else
    grep -qF -- "$word" "$work/changed.txt" || found=0
  fi
  [ "$value" = - ] || grep -qF -- "$value" "$work/changed.txt" || { found=0; word="$word' and '$value"; }
  if [ "$found" = 1 ]; then
    echo "  mentioned: $msg"
    continue
  fi
  echo "::error file=$changelog::flags-diff: $msg ($tag -> HEAD) and ### Changed does not mention '$word'" >&2
  missing=$((missing + 1))
done <"$work/problems.tsv"

if [ "$missing" -gt 0 ]; then
  echo "flags-diff: $missing CLI change(s) since $tag are not in $changelog; list each under ### Changed with a migration note" >&2
  exit 1
fi
if [ -s "$work/problems.tsv" ]; then
  echo "ok: every CLI change since $tag ($(wc -l <"$work/problems.tsv" | tr -d ' ')) is named under Changed in $changelog (all mentioned)"
else
  echo "ok: no flag, default or command changed since $tag"
fi
