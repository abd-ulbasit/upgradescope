#!/usr/bin/env bash
# Generates THIRD_PARTY_NOTICES, the license texts of everything a release
# binary contains that this repository did not write (make notices), or
# checks the committed file (make notices-check; CI's notices job):
#
#   - every Go module ./cmd/upgradescope links on linux, darwin and windows,
#     amd64 and arm64, found by go-licenses (pinned below), plus the Go
#     standard library;
#   - every production npm package of the dashboard (web/package-lock.json
#     entries not marked dev), with its license text from web/node_modules,
#     plus the build-tool code Vite injects into the bundle (BUNDLED_DEV).
#
# Each component's license must be on ALLOWED. Anything else fails: no
# license found ("Unknown"), copyleft (GPL, LGPL, AGPL, SSPL) and any
# license nobody has reviewed yet. Widen ALLOWED only in a reviewed PR.
#
# THIRD_PARTY_NOTICES is committed, like the dashboard bundle, so the
# from-source Dockerfile and `go install` checkouts have it; --check fails
# when it differs from a fresh generation (a dependency changed and nobody
# ran `make notices`). release.yml gates every tag on that check, and
# GoReleaser ships the file in every archive, package and image.
#
# Usage: hack/notices.sh [--check]
# Needs Go, jq and network (go-licenses reads module metadata), and Node
# when web/node_modules lacks a bundled package (it then runs npm ci).
#
# Knobs, for hack/notices_test.sh:
#   UPGRADESCOPE_NOTICES_ROOT   tree to work on (default: this repository)
#   UPGRADESCOPE_GO_LICENSES    command run as go-licenses (default: the pin)
#   UPGRADESCOPE_GO_LICENSE     the Go standard library's LICENSE file
set -euo pipefail
# Byte-identical output on every machine: sort and awk order by bytes.
export LC_ALL=C
cd "${UPGRADESCOPE_NOTICES_ROOT:-$(dirname "$0")/..}"

GO_LICENSES_VERSION=v2.0.1
ALLOWED="Apache-2.0 BSD-2-Clause BSD-3-Clause ISC MIT 0BSD Zlib"
# Dev dependencies whose code ends up in the bundle anyway: Vite inlines its
# modulepreload polyfill into the entry chunk.
BUNDLED_DEV="vite"
OUT=THIRD_PARTY_NOTICES

check=0
case "${1:-}" in
  --check) check=1 ;;
  '') ;;
  *) echo "usage: $0 [--check]" >&2; exit 2 ;;
esac

die() { echo "::error::notices: $*" >&2; exit 1; }
command -v jq >/dev/null || die "jq is required (brew install jq / apt install jq)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if [ -n "${UPGRADESCOPE_GO_LICENSES:-}" ]; then
  golicenses() { $UPGRADESCOPE_GO_LICENSES "$@"; }
else
  GOBIN="$tmp/bin" go install "github.com/google/go-licenses/v2@$GO_LICENSES_VERSION"
  golicenses() { "$tmp/bin/go-licenses" "$@"; }
fi

sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -c1-64; }

allowed() {
  local l
  for l in $ALLOWED; do [ "$1" = "$l" ] && return 0; done
  return 1
}

# components.tsv: name <TAB> version <TAB> license <TAB> license file
# (one row per license when a file carries several).
printf '{{ range . }}{{ .Name }}\t{{ .Version }}\t{{ .LicenseName }}\t{{ .LicensePath }}\n{{ end }}' >"$tmp/report.tpl"
: >"$tmp/go.tsv"
for goos in linux darwin windows; do
  for goarch in amd64 arm64; do
    GOOS=$goos GOARCH=$goarch golicenses report ./cmd/upgradescope \
      --ignore github.com/abd-ulbasit/upgradescope \
      --template "$tmp/report.tpl" >>"$tmp/go.tsv" 2>"$tmp/go-licenses.log" \
      || { cat "$tmp/go-licenses.log" >&2; die "go-licenses report failed for $goos/$goarch"; }
  done
done
sort -u "$tmp/go.tsv" -o "$tmp/go.tsv"
[ -s "$tmp/go.tsv" ] || die "go-licenses found no Go dependencies"

gostd="${UPGRADESCOPE_GO_LICENSE:-$(go env GOROOT)/LICENSE}"
goversion="go$(awk '/^go [0-9]/ {print $2; exit}' go.mod)"
[ -f "$gostd" ] || die "no Go LICENSE at $gostd"
printf 'Go standard library\t%s\tBSD-3-Clause\t%s\n' "$goversion" "$gostd" >>"$tmp/go.tsv"

# npm: production packages, and BUNDLED_DEV, from the lockfile.
lock=web/package-lock.json
[ -f "$lock" ] || die "no $lock"
jq -r --arg bundled "$BUNDLED_DEV" '
  ($bundled | split(" ")) as $b
  | .packages | to_entries[]
  | select(.key != "")
  | select((.value.dev != true and .value.devOptional != true) or ((.key | sub("^.*node_modules/"; "")) as $n | $b | index($n)))
  | [(.key | sub("^.*node_modules/"; "")), .value.version, (.value.license // "Unknown"), .key] | @tsv
' "$lock" | sort -u >"$tmp/npm.raw"
if cut -f4 "$tmp/npm.raw" | while read -r dir; do [ -d "web/$dir" ] || exit 1; done; then :; else
  echo "== npm ci in web/ (web/node_modules lacks a package the bundle includes)" >&2
  (cd web && npm ci --ignore-scripts --no-fund --no-audit >&2)
fi
: >"$tmp/npm.tsv"
while IFS=$'\t' read -r name version license dir; do
  file="$(find "web/$dir" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' \) 2>/dev/null | sort | head -1)"
  [ -n "$file" ] || file=UNKNOWN
  printf '%s\t%s\t%s\t%s\n' "$name" "$version" "$license" "$file" >>"$tmp/npm.tsv"
done <"$tmp/npm.raw"
[ -s "$tmp/npm.tsv" ] || die "no production npm packages in $lock"

# Policy: every component has a license text and an allowed license.
bad=0
while IFS=$'\t' read -r name version license file; do
  if [ "$file" = UNKNOWN ] || [ ! -f "$file" ]; then
    echo "::error::notices: $name $version: no license file found" >&2
    bad=1
  elif ! allowed "$license"; then
    echo "::error::notices: $name $version is licensed '$license', which is not allowed (ALLOWED: $ALLOWED); GPL, LGPL, AGPL, SSPL and unidentified licenses are never allowed" >&2
    bad=1
  fi
done < <(cat "$tmp/go.tsv" "$tmp/npm.tsv")
[ "$bad" = 0 ] || die "dependency license policy failed (see above)"

# One section per distinct license text, naming every component it covers
# (most Apache-2.0 modules ship the same text), in component order.
rule() { printf '%080d\n' 0 | tr 0 "$1"; }
section() { # $1 = components tsv, $2 = title
  local tsv=$1 title=$2 sum file
  rule =
  printf '%s\n' "$title"
  rule =
  printf '\nComponent list (name, version, license):\n\n'
  awk -F'\t' '{ k = $1 " " $2; l[k] = (k in l) ? l[k] " AND " $3 : $3 } END { for (k in l) printf "  %s (%s)\n", k, l[k] }' "$tsv" | sort
  printf '\n'
  # groups: sha256 of the license text <TAB> "name version" <TAB> file
  : >"$tmp/groups"
  while IFS=$'\t' read -r name version _ file; do
    sum="$(sha256 <"$file")"
    printf '%s\t%s %s\t%s\n' "$sum" "$name" "$version" "$file" >>"$tmp/groups"
  done < <(awk -F'\t' '!seen[$1 "\t" $2]++' "$tsv")
  sort -t$'\t' -k2,2 "$tmp/groups" | awk -F'\t' '!seen[$1]++ { print $1 "\t" $3 }' >"$tmp/order"
  while IFS=$'\t' read -r sum file; do
    rule -
    awk -F'\t' -v s="$sum" '$1 == s { print $2 }' "$tmp/groups" | sort -u
    rule -
    printf '\n'
    # CRLF and trailing whitespace normalized: the same text reads the
    # same whichever checkout or module cache it came from.
    tr -d '\r' <"$file" | sed 's/[[:space:]]*$//'
    printf '\n'
  done <"$tmp/order"
}

{
  cat <<'EOF'
THIRD-PARTY SOFTWARE NOTICES

upgradescope binaries statically link the Go modules and the Go standard
library listed below, and embed a dashboard bundle built from the npm
packages listed below. Each remains under its own license, reproduced in
full here. upgradescope itself is licensed under the Apache License 2.0
(LICENSE); see NOTICE for the third-party data in its knowledge base.

Generated by hack/notices.sh (make notices) from the package graph of
./cmd/upgradescope on linux, darwin and windows (amd64, arm64) and from
web/package-lock.json. Do not edit by hand.

EOF
  section "$tmp/go.tsv" "Go modules"
  section "$tmp/npm.tsv" "npm packages (dashboard bundle)"
} >"$tmp/$OUT"

if [ "$check" = 1 ]; then
  if ! diff -u "$OUT" "$tmp/$OUT" >"$tmp/diff"; then
    head -60 "$tmp/diff" >&2
    die "$OUT is stale: a dependency changed. Run 'make notices' and commit the result."
  fi
  echo "notices: $OUT is up to date ($(grep -c . "$tmp/go.tsv") Go, $(grep -c . "$tmp/npm.tsv") npm rows; licenses allowed)"
else
  mv "$tmp/$OUT" "$OUT"
  echo "notices: wrote $OUT"
fi
