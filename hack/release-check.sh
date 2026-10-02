#!/usr/bin/env bash
# Proves the packaging before a tag does (make release-check; CI's
# release-check job). Publishes and signs nothing:
#   1. `goreleaser check`: .goreleaser.yml is valid for the pinned GoReleaser;
#   2. `goreleaser release --snapshot`: every archive, deb/rpm/apk package
#      (and, with Docker, every per-arch image from Dockerfile.release) builds;
#   3. the archive names are the ones the Action (action/run.sh) downloads from
#      each release — a drift here 404s every Action consumer;
#   4. every archive carries the binary, LICENSE, NOTICE, THIRD_PARTY_NOTICES,
#      README.md, the four shell completions and the man pages; the deb and
#      apk packages install the binary, completions, man pages and license
#      files at their standard paths; every image has /licenses/ and the
#      full OCI labels;
#   5. checksums.txt lists every published contract in api/ (the JSON report
#      and webhook schemas, the OpenAPI document) with its sha256, so the
#      release attaches them (release.extra_files) under the signature (#60);
#   6. the binary and archive sizes README.md and docs/operations/install.md
#      state are within 2% of the ones just built (hack/check-doc-sizes.sh);
#   7. the binary for this machine is stamped (version, commit, commit date,
#      registry date) and serves the embedded dashboard at / with every
#      asset it references (hack/dashboard-smoke.sh).
#
# GoReleaser runs through `go run` at GORELEASER_VERSION (the Makefile pins
# it; make check-toolchain keeps release.yml on the same version).
#
# Knobs:
#   GORELEASER_VERSION  required (the Makefile passes its pin)
#   GORELEASER_SKIP     --skip list for the snapshot (default publish,sign,sbom;
#                       add ,docker on a machine without a Docker engine)
set -euo pipefail
cd "$(dirname "$0")/.."

: "${GORELEASER_VERSION:?set GORELEASER_VERSION (make release-check passes the Makefile pin)}"
SKIP=${GORELEASER_SKIP:-publish,sign,sbom}
goreleaser() { go run "github.com/goreleaser/goreleaser/v2@$GORELEASER_VERSION" "$@"; }

die() { echo "::error::release-check: $*" >&2; exit 1; }
command -v jq >/dev/null || die "jq is required (brew install jq / apt install jq)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "== goreleaser $GORELEASER_VERSION check"
goreleaser check

echo "== goreleaser $GORELEASER_VERSION release --snapshot --clean --skip=$SKIP"
goreleaser release --snapshot --clean --skip="$SKIP"

echo "== archive names match what action/run.sh downloads"
template="$(sed -n 's/^ *asset="\(.*\)"$/\1/p' action/run.sh)"
[ -n "$template" ] || die "no asset=\"...\" line in action/run.sh"
for os in linux darwin; do
  for arch in amd64 arm64; do
    name="${template//\$\{os\}/$os}"
    name="${name//\$\{arch\}/$arch}"
    [ -f "dist/$name" ] || { ls dist >&2; die "action/run.sh expects dist/$name, goreleaser did not produce it"; }
    echo "ok: $name"
  done
done
for arch in amd64 arm64; do
  [ -f "dist/upgradescope_windows_${arch}.zip" ] || die "no windows $arch zip"
  echo "ok: upgradescope_windows_${arch}.zip"
done

echo "== every archive carries the binary, licenses, completions and man pages"
want_common="LICENSE NOTICE THIRD_PARTY_NOTICES README.md completions/upgradescope.bash completions/upgradescope.zsh completions/upgradescope.fish completions/upgradescope.ps1 manpages/upgradescope.1.gz manpages/upgradescope-scan.1.gz"
n=0
for a in dist/upgradescope_*.tar.gz dist/upgradescope_*.zip; do
  [ -f "$a" ] || continue
  n=$((n + 1))
  case "$a" in
    *.zip) unzip -Z1 "$a" >"$tmp/list"; bin=upgradescope.exe ;;
    *) tar -tzf "$a" >"$tmp/list"; bin=upgradescope ;;
  esac
  for f in $bin $want_common; do
    grep -qx "$f" "$tmp/list" || { cat "$tmp/list" >&2; die "$a lacks $f"; }
  done
  echo "ok: $a ($(grep -c . "$tmp/list") files)"
done
[ "$n" = 6 ] || die "found $n archives, want 6 (linux, darwin, windows x amd64, arm64)"

echo "== deb, rpm and apk packages for linux amd64 and arm64"
jq -r '.[] | select(.type == "Linux Package") | .path' dist/artifacts.json | sort >"$tmp/pkgs"
cat "$tmp/pkgs"
for fmt in deb rpm apk; do
  [ "$(grep -c "\.$fmt\$" "$tmp/pkgs" || true)" = 2 ] || die "want 2 .$fmt packages (amd64, arm64)"
done
want_pkg="usr/bin/upgradescope usr/share/bash-completion/completions/upgradescope usr/share/fish/vendor_completions.d/upgradescope.fish usr/share/man/man1/upgradescope.1.gz usr/share/man/man1/upgradescope-scan.1.gz usr/share/doc/upgradescope/LICENSE usr/share/doc/upgradescope/NOTICE usr/share/doc/upgradescope/THIRD_PARTY_NOTICES"
while read -r p; do
  case "$p" in
    # A deb is an ar archive around data.tar.gz; an apk is concatenated
    # gzipped tars. rpm needs rpm tooling, so only its presence is checked.
    *.deb) (cd "$tmp" && rm -f data.tar.* && ar x "$OLDPWD/$p" && tar -tzf data.tar.gz) | sed 's#^\./##' >"$tmp/list"; zsh=usr/share/zsh/vendor-completions/_upgradescope ;;
    *.apk) tar -tzf "$p" 2>/dev/null | sed 's#^\./##' >"$tmp/list"; zsh=usr/share/zsh/site-functions/_upgradescope ;;
    *) continue ;;
  esac
  for f in $want_pkg $zsh; do
    grep -qx "$f" "$tmp/list" || { cat "$tmp/list" >&2; die "$p lacks /$f"; }
  done
  echo "ok: $p installs the binary, completions, man pages and license files"
done <"$tmp/pkgs"

case ",$SKIP," in
  *,docker,*) echo "== images skipped (GORELEASER_SKIP has docker)" ;;
  *)
    echo "== every image carries /licenses and the OCI labels"
    jq -r '.[] | select(.type == "Docker Image") | .name' dist/artifacts.json | sort -u >"$tmp/images"
    [ -s "$tmp/images" ] || die "no Docker Image in dist/artifacts.json"
    while read -r img; do
      for l in title description url documentation source vendor licenses revision version created; do
        got="$(docker image inspect --format "{{ index .Config.Labels \"org.opencontainers.image.$l\" }}" "$img")"
        [ -n "$got" ] || die "$img has no org.opencontainers.image.$l label"
      done
      cid="$(docker create "$img")"
      rm -rf "$tmp/licenses"
      docker cp "$cid:/licenses" "$tmp/licenses" >/dev/null
      docker rm "$cid" >/dev/null
      for f in LICENSE NOTICE THIRD_PARTY_NOTICES; do
        cmp -s "$tmp/licenses/$f" "$f" || die "$img: /licenses/$f is missing or differs from the repository's"
      done
      echo "ok: $img (/licenses, OCI labels)"
    done <"$tmp/images"
    ;;
esac

echo "== checksums.txt covers the published contracts in api/ (#60)"
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'; }
[ -f dist/checksums.txt ] || die "no dist/checksums.txt"
for f in api/*; do
  name=${f##*/}
  got=$(awk -v f="$name" '$2 == f { print $1 }' dist/checksums.txt)
  [ -n "$got" ] || { cat dist/checksums.txt >&2; die "dist/checksums.txt has no $name: add $f to checksum.extra_files in .goreleaser.yml"; }
  [ "$got" = "$(sha256 "$f")" ] || die "dist/checksums.txt lists $name as $got, which is not the sha256 of $f"
  echo "ok: $name"
done

echo "== the sizes the docs state are the build's (within 2%)"
DIST=dist hack/check-doc-sizes.sh

echo "== the release binary for this machine is stamped and serves the dashboard at /"
goos=$(go env GOOS)
goarch=$(go env GOARCH)
bin=$(jq -r --arg os "$goos" --arg arch "$goarch" \
  '.[] | select(.type == "Binary" and .goos == $os and .goarch == $arch) | .path' dist/artifacts.json | head -1)
[ -n "$bin" ] && [ -x "$bin" ] || die "no $goos/$goarch binary in dist/artifacts.json"
"$bin" --version
info="$("$bin" version --output json)"
[ "$(jq -r .commit <<<"$info")" = "$(git rev-parse HEAD)" ] || die "binary commit is not HEAD: $info"
[ "$(jq -r .buildDate <<<"$info")" = "$(TZ=UTC git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd)" ] \
  || die "binary build date is not the commit date: $info"
[ "$(jq -r .registryDate <<<"$info")" = "$(git log -1 --format=%cs -- registry/data)" ] \
  || die "binary registry date is not the last registry/data commit's: $info"
echo "ok: version, commit, commit date and registry date stamped"
DASHBOARD_SMOKE_PORT=18431 hack/dashboard-smoke.sh "$bin"
echo "release-check: OK"
