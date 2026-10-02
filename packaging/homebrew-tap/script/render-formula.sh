#!/usr/bin/env bash
# Renders Formula/upgradescope.rb from formula.rb.tmpl for one upgradescope
# release: the version, and the sha256 of each macOS/Linux archive as the
# release's checksums.txt lists it. Verify checksums.txt's signature before
# rendering (.github/workflows/update.yml does): the formula trusts it.
#
# Usage: script/render-formula.sh <X.Y.Z> <checksums.txt> [out]
#   out defaults to Formula/upgradescope.rb; "-" writes to stdout.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "render-formula: $*" >&2; exit 1; }
[ $# -ge 2 ] || die "usage: $0 <X.Y.Z> <checksums.txt> [out]"
version=$1 checksums=$2 out=${3:-Formula/upgradescope.rb}
printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' \
  || die "'$version' is not X.Y.Z (the tap tracks stable releases only)"
[ -f "$checksums" ] || die "no $checksums"

sha() { # sha <os> <arch>: the archive's sha256, exactly one line, 64 hex
  local name="upgradescope_$1_$2.tar.gz" got
  got="$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1 }' "$checksums")"
  [ "$(grep -c . <<<"$got")" = 1 ] && printf '%s' "$got" | grep -Eq '^[a-f0-9]{64}$' \
    || die "$checksums has no single sha256 for $name"
  printf '%s' "$got"
}

# Assigned one by one: a failure inside $(...) in a command's arguments
# would not stop the script.
darwin_arm64=$(sha darwin arm64)
darwin_amd64=$(sha darwin amd64)
linux_arm64=$(sha linux arm64)
linux_amd64=$(sha linux amd64)
rendered="$(sed \
  -e "s/@VERSION@/$version/g" \
  -e "s/@SHA256_DARWIN_ARM64@/$darwin_arm64/" \
  -e "s/@SHA256_DARWIN_AMD64@/$darwin_amd64/" \
  -e "s/@SHA256_LINUX_ARM64@/$linux_arm64/" \
  -e "s/@SHA256_LINUX_AMD64@/$linux_amd64/" \
  formula.rb.tmpl)"
! grep -q '@[A-Z0-9_]*@' <<<"$rendered" || die "unrendered placeholder left in the formula"

if [ "$out" = - ]; then
  printf '%s\n' "$rendered"
else
  mkdir -p "$(dirname "$out")"
  printf '%s\n' "$rendered" >"$out"
  echo "render-formula: $out -> $version" >&2
fi
