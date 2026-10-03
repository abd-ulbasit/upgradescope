#!/usr/bin/env bash
# Every file named in checksums.txt must keep its name when GitHub serves it
# (hack/release-check.sh; #198). GitHub rewrites characters outside
# [A-Za-z0-9._-] in an asset name (`~` becomes `.`), so a checksums.txt line
# for the renamed file never matches the download and
# `sha256sum --ignore-missing -c checksums.txt` skips it without a word.
# nfpm's conventional pre-release names (upgradescope_0.2.0~rc.2_amd64.deb)
# did exactly that; .goreleaser.yml now writes them with `.`.
#
# Usage: hack/check-asset-names.sh [dist/checksums.txt]
# Offline; hack/check-asset-names_test.sh drives it.
set -euo pipefail

file=${1:-dist/checksums.txt}
die() { echo "::error file=$file::asset-names: $*" >&2; exit 1; }
[ -f "$file" ] || die "no $file"

n=0
while read -r _ name; do
  [ -n "$name" ] || continue
  n=$((n + 1))
  case "$name" in
    *[!A-Za-z0-9._-]*) die "'$name' would be renamed by GitHub on upload, so its checksums.txt line would never match the download; change its file_name_template" ;;
  esac
done <"$file"
[ "$n" -gt 0 ] || die "$file lists no files"
echo "ok: $n asset names survive upload unchanged"
