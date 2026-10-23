#!/usr/bin/env bash
# Tests that the release workflow builds and signs in separate jobs
# (make hack-test). The goreleaser job compiles every module the binary links
# and runs repository code, so a malicious dependency runs there; it must not
# be able to sign or attest anything as the release workflow:
#   - the goreleaser job holds no id-token or attestations permission, has no
#     cosign step, and .goreleaser.yml has no signs: or docker_signs:;
#   - it is gated by the `release` environment and reports what it built
#     (checksums.txt's sha256, the image index digest) as job outputs;
#   - the sign job needs it, holds id-token and attestations, builds nothing
#     (no setup-go/setup-node/GoReleaser, no go, make, npm or docker build),
#     checks the release's checksums.txt against the goreleaser job's sha256
#     before any cosign step (run here against a stub gh and docker: a
#     replaced checksums.txt fails the step), and attests the image with
#     push-to-registry;
#   - only the sign and chart jobs can mint an OIDC token, and the chart and
#     verify jobs run after sign;
#   - the chart and verify jobs use the image digest the goreleaser job
#     reported (what sign signed), never a fresh lookup of the tag, and stop
#     when a release tag no longer names it (run here against a stub docker).
# Offline; needs awk, grep and sha256sum or shasum.
set -euo pipefail
cd "$(dirname "$0")/.."

wf=${RELEASE_WORKFLOW:-.github/workflows/release.yml}
gr=${GORELEASER_CONFIG:-.goreleaser.yml}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}
check() { if eval "$2"; then ok "$1"; else fail "$1"; fi; }

# job <name>: the lines of the top-level job <name> (comments dropped).
job() { awk -v want="  $1:" '$0 == want { on = 1; next } on && /^  [a-z0-9_-]+:/ { exit } on && !/^ *#/ { print }' "$wf"; }
# perms <name>: "scope access" lines of the job's permissions block.
perms() { job "$1" | awk '/^    permissions:/ { p = 1; next } p && /^      [a-z-]+:/ { s = $1; sub(/:$/, "", s); print s, $2; next } p && !/^      / { p = 0 }'; }
# step_run <job> <name>: the `run: |` block of that job's step, dedented.
step_run() {
  job "$1" | awk -v want="- name: $2" '
    { line = $0; sub(/^ +/, "", line) }
    line ~ /^- / { inside = (line == want); inrun = 0; next }
    inside && line == "run: |" { match($0, /^ */); ind = RLENGTH; inrun = 1; next }
    inrun {
      match($0, /^ */)
      if ($0 !~ /^ *$/ && RLENGTH <= ind) { inrun = 0; inside = 0; next }
      print substr($0, ind + 3)
    }
  '
}
# line_of <job> <fixed string>: the first line number of it within the job.
line_of() { job "$1" | grep -nF -- "$2" | head -1 | cut -d: -f1; }

gj=$(job goreleaser)
sj=$(job sign)
[ -n "$gj" ] || { echo "FAIL no goreleaser job in $wf" >&2; exit 1; }
[ -n "$sj" ] || { echo "FAIL no sign job in $wf" >&2; exit 1; }

# --- the goreleaser job: builds and publishes, cannot sign -------------------

perms goreleaser >"$work/gperms"
check "goreleaser holds no id-token permission (it cannot mint an OIDC token)" "! grep -q '^id-token ' '$work/gperms'"
check "goreleaser holds no attestations permission" "! grep -q '^attestations ' '$work/gperms'"
check "goreleaser holds contents: write and packages: write (it publishes)" \
  "grep -qx 'contents write' '$work/gperms' && grep -qx 'packages write' '$work/gperms'"
check "goreleaser installs no cosign and runs none" "! grep -qE 'cosign-installer@|(^|[^a-z-])cosign ' <<<\"\$gj\""
nonc_gr=$(grep -vE '^ *#' "$gr")
check ".goreleaser.yml has no signs: (GoReleaser signs nothing)" "! grep -qE '^signs:' <<<\"\$nonc_gr\""
check ".goreleaser.yml has no docker_signs:" "! grep -qE '^docker_signs:' <<<\"\$nonc_gr\""
check "goreleaser waits for the release environment" "grep -qxE '    environment: (release|\\{ *name: *release *\\})' <<<\"\$gj\""
check "goreleaser outputs checksums-sha256 and image-digest from its built step" \
  "grep -qxF '      checksums-sha256: \${{ steps.built.outputs.checksums-sha256 }}' <<<\"\$gj\" &&
   grep -qxF '      image-digest: \${{ steps.built.outputs.image-digest }}' <<<\"\$gj\""
gl=$(line_of goreleaser 'uses: goreleaser/goreleaser-action@')
bl=$(line_of goreleaser 'id: built')
check "the built step runs after GoReleaser" "[ -n '$gl' ] && [ -n '$bl' ] && [ '$gl' -lt '$bl' ]"

# --- the sign job: signs and attests, builds nothing ------------------------

perms sign >"$work/sperms"
check "sign needs goreleaser" "grep -qxE '    needs: (goreleaser|\\[goreleaser\\]|\\[[a-z, -]*goreleaser[a-z, -]*\\])' <<<\"\$sj\""
check "sign holds id-token: write and attestations: write" \
  "grep -qx 'id-token write' '$work/sperms' && grep -qx 'attestations write' '$work/sperms'"
check "sign sets up no toolchain (setup-go, setup-node, GoReleaser, setup-buildx)" \
  "! grep -qE 'actions/setup-go@|actions/setup-node@|goreleaser/goreleaser-action@|docker/setup-buildx-action@|download-syft' <<<\"\$sj\""
# Commands that would build or run dependency code, at the start of a line or
# after a shell separator.
check "sign runs no go, make, npm, goreleaser or docker build" \
  "! grep -qE '(^|[;&|(]|\\\$\\()[[:space:]]*(go|make|npm|npx|goreleaser)[[:space:]]|docker[[:space:]]+(buildx[[:space:]]+)?build([[:space:]]|\$)' <<<\"\$(grep -vE '^ *(- )?(uses|name|with|env|id|if|needs|runs-on|timeout-minutes|permissions):' <<<\"\$sj\")\""
vname="checksums.txt and the image are the ones the goreleaser job built"
vl=$(line_of sign "- name: $vname")
cl=$(job sign | grep -nE '^[^#]*cosign (sign|sign-blob|attest)' | head -1 | cut -d: -f1)
al=$(line_of sign 'uses: actions/attest-build-provenance@')
check "the checksum check runs before any cosign signing and any attestation" \
  "[ -n '$vl' ] && [ -n '$cl' ] && [ -n '$al' ] && [ '$vl' -lt '$cl' ] && [ '$vl' -lt '$al' ]"
check "sign reads what to sign from the goreleaser job's outputs" \
  "grep -qxF '      CHECKSUMS_SHA256: \${{ needs.goreleaser.outputs.checksums-sha256 }}' <<<\"\$sj\" &&
   grep -qxF '      IMAGE_DIGEST: \${{ needs.goreleaser.outputs.image-digest }}' <<<\"\$sj\""
check "sign signs the image by the reported digest, recursively" "grep -qF 'cosign sign --yes --recursive \"\$IMAGE@\$IMAGE_DIGEST\"' <<<\"\$sj\""
check "the image is attested by that digest and the attestation pushed to the registry" \
  "awk '/uses: actions\\/attest-build-provenance@/ { a = 1; d = 0; p = 0; next } a && /^      - / { a = 0 } a && /subject-digest: \\\$\\{\\{ needs.goreleaser.outputs.image-digest \\}\\}/ { d = 1 } a && /push-to-registry: true/ { p = 1 } a && d && p { f = 1 } END { exit !f }' <<<\"\$sj\""

# The checksum check, run as Actions runs a run: step (bash -e), against a
# stub gh that serves a checksums.txt and a stub docker.
step_run sign "$vname" >"$work/verify.sh"
[ -s "$work/verify.sh" ] || { echo "FAIL no '$vname' step in the sign job" >&2; exit 1; }
mkdir -p "$work/bin"
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >>"$CALLS"
[ "$1 $2" = "release download" ] || exit 2
dir=
while [ $# -gt 0 ]; do [ "$1" = -D ] && dir=$2; shift; done
cp "$STUB_CHECKSUMS" "$dir/checksums.txt"
EOF
cat >"$work/bin/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >>"$CALLS"
EOF
cat >"$work/bin/cosign" <<'EOF'
#!/usr/bin/env bash
echo "cosign $*" >>"$CALLS"
EOF
chmod +x "$work/bin/"*
sha() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'; }
printf 'aaaa  upgradescope_linux_amd64.tar.gz\n' >"$work/built.txt"
printf 'bbbb  upgradescope_linux_amd64.tar.gz\n' >"$work/replaced.txt"
digest="sha256:$(printf '%064d' 7)"
# run_verify <checksums.txt the release serves> [CHECKSUMS_SHA256] [IMAGE_DIGEST]
run_verify() {
  : >"$work/calls"
  mkdir -p "$work/tmp"
  rm -f "$work/tmp/sign/checksums.txt"
  code=0
  env PATH="$work/bin:$PATH" CALLS="$work/calls" STUB_CHECKSUMS="$1" RUNNER_TEMP="$work/tmp" \
    GITHUB_REF_NAME=v9.9.9 GITHUB_REPOSITORY=o/r IMAGE=ghcr.io/o/r GH_TOKEN=stub \
    CHECKSUMS_SHA256="${2-$(sha "$work/built.txt")}" IMAGE_DIGEST="${3-$digest}" \
    bash -e "$work/verify.sh" >"$work/out" 2>&1 || code=$?
}
run_verify "$work/built.txt"
check "the checksums.txt the goreleaser job built passes the check" "[ '$code' = 0 ]"
run_verify "$work/replaced.txt"
if [ "$code" != 0 ] && grep -q "is not the one the goreleaser job built" "$work/out"; then
  ok "a checksums.txt replaced on the release after the build fails the check"
else
  fail "a replaced checksums.txt passed the check (exit $code)" "$work/out"
fi
run_verify "$work/built.txt" ""
check "a missing checksums-sha256 output fails the check" "[ '$code' != 0 ] && grep -q 'reported no checksums.txt sha256' '$work/out'"
run_verify "$work/built.txt" "$(sha "$work/built.txt")" "latest"
check "an image digest that is not sha256:<64 hex> fails the check" "[ '$code' != 0 ] && grep -q 'reported no image digest' '$work/out'"
check "the check itself signs nothing" "! grep -q '^cosign ' '$work/calls'"

# --- who else can mint an OIDC token, and the order after sign ---------------

jobs=$(awk '/^jobs:/ { j = 1; next } j && /^  [a-z0-9_-]+:[ ]*(#.*)?$/ { s = $1; sub(/:$/, "", s); print s }' "$wf")
oidc=$(for j in $jobs; do if perms "$j" | grep -qx 'id-token write'; then echo "$j"; fi; done | sort | tr '\n' ' ' | sed 's/ $//')
check "only the sign and chart jobs can mint an OIDC token (got: $oidc)" "[ '$oidc' = 'chart sign' ]"
check "the top-level permissions grant no id-token" "! awk '/^permissions:/ { p = 1; next } p && /^[^ ]/ { p = 0 } p' '$wf' | grep -q 'id-token'"
for j in chart verify; do
  check "$j runs after sign" "job $j | grep -E '^    needs:' | grep -qE '[[, ]sign[], ]|: sign\$'"
done

# --- chart and verify: the digest that was signed, not the tag's ----------

tname="the release tags still name the image the sign job signed"
# The stub docker: `docker buildx imagetools inspect <image>:<tag> --format ...`
# prints an index whose digest is $STUB_TAG_<tag, with . and - as _>, or
# fails as for a tag that does not exist.
cat >"$work/bin/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >>"$CALLS"
tag=${4##*:}
var="STUB_TAG_$(printf '%s' "$tag" | tr '.-' '__')"
[ -n "${!var:-}" ] || { echo "ERROR: $4: not found" >&2; exit 1; }
printf '{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"%s","size":1}\n' "${!var}"
EOF
chmod +x "$work/bin/docker"
other="sha256:$(printf '%064d' 8)"
want_env='      IMAGE_DIGEST: ${{ needs.goreleaser.outputs.image-digest }}'
# run_tags <job> <IMAGE_DIGEST> <digest vX.Y.Z names> <digest X.Y.Z names>
run_tags() {
  : >"$work/calls"
  code=0
  env PATH="$work/bin:$PATH" CALLS="$work/calls" GITHUB_REF_NAME=v9.9.9 VERSION=9.9.9 IMAGE=ghcr.io/o/r \
    IMAGE_DIGEST="$2" STUB_TAG_v9_9_9="$3" STUB_TAG_9_9_9="$4" \
    bash -e "$work/tags-$1.sh" >"$work/out" 2>&1 || code=$?
}
for j in chart verify; do
  job "$j" >"$work/job-$j"
  if grep -qxF -- "$want_env" "$work/job-$j"; then ok "$j reads the image digest from the goreleaser job's outputs"; else
    fail "$j has no '$want_env'"; fi
  if ! grep -qE 'digest="?\$\(docker buildx imagetools inspect' "$work/job-$j"; then ok "$j never takes a digest from a lookup of the tag"; else
    fail "$j still looks a digest up by tag" "$work/job-$j"; fi
  tl=$(grep -nF -- "- name: $tname" "$work/job-$j" | head -1 | cut -d: -f1 || true)
  if [ -n "$tl" ] && ! sed -n "1,${tl}p" "$work/job-$j" | grep -qE '"\$IMAGE_DIGEST"|@\$IMAGE_DIGEST|\$digest'; then
    ok "$j checks the tags before any step uses the digest"
  else
    fail "$j uses the digest before (or without) the '$tname' step" "$work/job-$j"
  fi
  step_run "$j" "$tname" >"$work/tags-$j.sh"
  [ -s "$work/tags-$j.sh" ] || { fail "no '$tname' step in the $j job"; continue; }
  run_tags "$j" "$digest" "$digest" "$digest"
  if [ "$code" = 0 ] && grep -q 'ghcr.io/o/r:v9.9.9 ' "$work/calls" && grep -q 'ghcr.io/o/r:9.9.9 ' "$work/calls"; then
    ok "$j: both release tags naming the signed index pass"; else fail "$j: both tags naming the signed index (exit $code)" "$work/out"; fi
  run_tags "$j" "$digest" "$other" "$digest"
  if [ "$code" != 0 ] && grep -qF "ghcr.io/o/r:v9.9.9 names $other, not $digest" "$work/out"; then
    ok "$j: a vX.Y.Z tag moved to another index fails"; else fail "$j: a moved vX.Y.Z tag passed (exit $code)" "$work/out"; fi
  run_tags "$j" "$digest" "$digest" "$other"
  if [ "$code" != 0 ] && grep -qF "ghcr.io/o/r:9.9.9 names $other" "$work/out"; then
    ok "$j: an X.Y.Z tag moved to another index fails"; else fail "$j: a moved X.Y.Z tag passed (exit $code)" "$work/out"; fi
  run_tags "$j" "$digest" "" "$digest"
  if [ "$code" != 0 ] && grep -qF 'names nothing' "$work/out"; then
    ok "$j: a tag that no longer resolves fails"; else fail "$j: a missing tag passed (exit $code)" "$work/out"; fi
  run_tags "$j" "" "$digest" "$digest"
  if [ "$code" != 0 ] && grep -qF 'reported no image digest' "$work/out"; then
    ok "$j: a missing image-digest output fails"; else fail "$j: a missing image-digest output passed (exit $code)" "$work/out"; fi
done

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "release-sign_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
