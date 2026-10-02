#!/usr/bin/env bash
# Tests for hack/vuln-latest-release.sh (make hack-test): which release it
# trusts before scanning. A copy of the script runs in a scratch tree whose
# hack/vulncheck.sh is a stub (exit $STUB_VULN), with stub curl (serving
# fixture releases), cosign and gh. Releases before $first_signed are
# unsigned and scanned on their sha256 alone; from it on, a missing or
# undownloadable signature bundle is a broken scan (exit 2), never a quiet
# downgrade to an unverified binary. Offline.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"

mkdir -p "$work/repo/hack" "$work/bin" "$work/rel"
cp hack/vuln-latest-release.sh "$work/repo/hack/"
printf '#!/bin/sh\necho "vulncheck: stub on $UPGRADESCOPE_VULN_BINS"\nexit "$STUB_VULN"\n' >"$work/repo/hack/vulncheck.sh"
printf '#!/bin/sh\nexit 0\n' >"$work/bin/gh"
printf '#!/bin/sh\necho "cosign $*" >>"%s/cosign.log"\nexit "$STUB_COSIGN"\n' "$work" >"$work/bin/cosign"

# curl: -o <file>, optional -w (prints the status), the URL last. A release
# asset is $work/rel/<tag>/<name>; <name>.status overrides the status
# (default 200 when the file exists, else 404). -f fails on >= 400.
cat >"$work/bin/curl" <<EOF
#!/usr/bin/env bash
out= w= f=
while [ \$# -gt 1 ]; do
  case "\$1" in -o) out=\$2; shift ;; -w) w=\$2; shift ;; -*f*) f=1 ;; esac
  shift
done
url=\$1 name=\${1##*/} tag=\${1%/*}; tag=\${tag##*/}
src="$work/rel/\$tag/\$name" code=404
[ -f "\$src" ] && code=200
[ -f "\$src.status" ] && code=\$(cat "\$src.status")
if [ "\$code" = 200 ]; then cp "\$src" "\$out"; else echo "error \$code" >"\$out"; fi
[ -z "\$w" ] || printf '%s' "\$code"
[ -n "\$f" ] && [ "\$code" -ge 400 ] && exit 22
exit 0
EOF
chmod +x "$work/repo/hack/"*.sh "$work/bin/"*

# release <tag> [bundle]: a release whose archive holds a stub binary, and
# checksums.txt.sigstore.json when bundle=yes.
release() {
  local d="$work/rel/$1"
  mkdir -p "$d/x"
  printf 'binary %s\n' "$1" >"$d/x/upgradescope"
  tar -czf "$d/upgradescope_linux_amd64.tar.gz" -C "$d/x" upgradescope
  (cd "$d" && shasum -a 256 upgradescope_linux_amd64.tar.gz >checksums.txt)
  [ "${2:-}" != yes ] || echo '{}' >"$d/checksums.txt.sigstore.json"
}
release v0.1.1
release v0.2.0 yes
release v0.2.1
release v0.3.0 yes
echo 503 >"$work/rel/v0.3.0/checksums.txt.sigstore.json.status"
release v0.1.0
echo 503 >"$work/rel/v0.1.0/checksums.txt.sigstore.json.status"
release v0.4.0 yes
echo 'bad' >>"$work/rel/v0.4.0/upgradescope_linux_amd64.tar.gz"

# expect <name> <tag> <cosign-exit> <want-exit> <substring> [no-cosign]
expect() {
  local name=$1 tag=$2 cosign=$3 want=$4 needle=$5 path="$work/bin:$PATH" got=0
  if [ "${6:-}" = no-cosign ]; then
    mkdir -p "$work/nocosign" && cp "$work/bin/gh" "$work/bin/curl" "$work/nocosign/"
    path="$work/nocosign:/usr/bin:/bin"
  fi
  PATH="$path" UPGRADESCOPE_RELEASE_TAG=$tag STUB_COSIGN=$cosign STUB_VULN=1 \
    "$work/repo/hack/vuln-latest-release.sh" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

expect "a signed release that verifies is scanned" v0.2.0 0 1 "checksums.txt signature verified"
expect "a signed release that does not verify is not scanned" v0.2.0 1 2 "does not verify against the release workflow's signature"
expect "an unsigned release before the first signed one is scanned on its sha256" v0.1.1 0 1 "predates signed releases"
expect "a release after the first signed one without a bundle is not scanned" v0.2.1 0 2 "has no checksums.txt.sigstore.json"
expect "a bundle that cannot be downloaded is not skipped" v0.3.0 0 2 "cannot download checksums.txt.sigstore.json of v0.3.0 (HTTP 503)"
expect "an old release whose bundle download fails is not skipped either" v0.1.0 0 2 "(HTTP 503)"
expect "an archive that does not match checksums.txt is not scanned" v0.4.0 0 2 "does not match checksums.txt"
expect "without cosign the scan says it is unverified" v0.2.0 0 1 "not signature-checked (cosign is not installed)" no-cosign

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "vuln-latest-release_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
