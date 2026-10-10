#!/usr/bin/env bash
# Tests for hack/pg-images.sh and the images CI pulls by service or
# container (make hack-test, #244). postgres:<major> was pulled by its moving
# tag, in the release-gating run too, so the gate's verdict could change
# with no commit. This checks:
#   - hack/pg-images.sh's table validation and matrices, on fixtures and on
#     hack/pg-images.txt (every row pinned by digest);
#   - ci.yml's pg-matrix step, run as Actions runs it for each kind of run:
#     17 on PRs, pushes and releases, every major on the schedule and on
#     demand;
#   - every services: or container: image in .github/workflows is pinned by
#     @sha256:, or is mirror.gcr.io/library/${{ matrix.image }} of a matrix
#     pg-matrix builds from the table (a service starts before any step, so
#     it is pulled from the mirror by name; hack/dockerhub-mirror_test.sh
#     covers the mirror itself), whose line is the only zizmor ignore in
#     the workflows.
# Offline; needs jq.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}

d17=sha256:2d2b8998d31037bf721cfdf764d76ba74171b4fab3431b7f72c27c56ddbdf9e3
d14=sha256:14bfab572eec6abf65892e1db7c3ba8d41b2a2855c1b143664907d6e146eb6e1
cat >"$work/table.txt" <<EOF
# comment
14 postgres:14@$d14 weekly

17 postgres:17@$d17 pr
EOF
# expect <name> <want-exit> <want-output> <table> <args...>
expect() {
  local name=$1 want=$2 needle=$3 table=$4 got=0
  shift 4
  PG_IMAGES_TABLE=$table hack/pg-images.sh "$@" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then ok "$name"; else
    echo "(exit $got, want $want and '$needle')" >>"$work/out"
    fail "$name" "$work/out"
  fi
}
expect "pr matrix is the pr rows" 0 "[{\"pg\":\"17\",\"image\":\"postgres:17@$d17\"}]" "$work/table.txt" matrix pr
expect "full matrix is every row, in order" 0 \
  "[{\"pg\":\"14\",\"image\":\"postgres:14@$d14\"},{\"pg\":\"17\",\"image\":\"postgres:17@$d17\"}]" "$work/table.txt" matrix all
expect "an unknown set fails" 2 usage "$work/table.txt" matrix nightly
# bad_row <name> <row> <want message>
bad_row() {
  printf '%s\n' "$2" >"$work/bad.txt"
  expect "$1" 2 "$3" "$work/bad.txt" matrix all
}
bad_row "a tag without a digest is refused" "17 postgres:17 pr" "is not postgres:17 pinned by digest"
bad_row "a digest of another major is refused" "17 postgres:16@$d17 pr" "is not postgres:17 pinned by digest"
bad_row "a short digest is refused" "17 postgres:17@sha256:abc pr" "is not postgres:17 pinned by digest"
bad_row "an -alpine or other variant is refused (one image per major)" "17 postgres:17-alpine@$d17 pr" "is not postgres:17 pinned by digest"
bad_row "a bad schedule is refused" "17 postgres:17@$d17 nightly" "schedule 'nightly' must be pr or weekly"
bad_row "a bad major is refused" "x17 postgres:17@$d17 pr" "bad major 'x17'"
: >"$work/empty.txt"
expect "an empty table is refused" 2 "no images" "$work/empty.txt" matrix all

# The real table.
if all=$(hack/pg-images.sh matrix all) && pr=$(hack/pg-images.sh matrix pr); then
  ok "hack/pg-images.txt is valid ($(jq length <<<"$all") majors)"
  jq -e 'map(.pg) | index("17") != null' <<<"$pr" >/dev/null && ok "PRs run Postgres 17" || fail "PRs do not run Postgres 17: $pr"
  jq -e 'map(.pg) == ["14","15","16","17","18"]' <<<"$all" >/dev/null &&
    ok "the full matrix is the supported range 14-18" || fail "the full matrix is not 14-18: $all"
else
  hack/pg-images.sh matrix all >"$work/out" 2>&1 || true
  fail "hack/pg-images.txt is valid" "$work/out"
fi

# --- ci.yml: pg-matrix and pg-conformance ------------------------------------

ci=.github/workflows/ci.yml
job() { awk -v want="  $1:" '$0 == want { on = 1; next } on && /^  [a-z0-9_-]+:/ { exit } on { print }' "$ci"; }
job pg-matrix >"$work/pg-matrix.job"
job pg-conformance >"$work/pg-conformance.job"
awk '/^        run: \|$/ { r = 1; next } r { if ($0 !~ /^ *$/ && $0 !~ /^          /) exit; print substr($0, 11) }' \
  "$work/pg-matrix.job" >"$work/matrix.sh"
[ -s "$work/matrix.sh" ] || { echo "FAIL no run block in ci.yml's pg-matrix job" >&2; exit 1; }
grep -qxF '          EVENT: ${{ github.event_name }}' "$work/pg-matrix.job" &&
  grep -qxF '          RELEASE: ${{ inputs.release == true }}' "$work/pg-matrix.job" &&
  grep -qxF '          FULL_MATRIX: ${{ inputs.full-matrix }}' "$work/pg-matrix.job" &&
  ok "pg-matrix takes the event, release and full-matrix as the kube matrix does" ||
  fail "pg-matrix's env is not EVENT, RELEASE and FULL_MATRIX from github.event_name and inputs"
# majors <event> <release> <full-matrix>: what the step outputs, as "14 15 ...".
majors() {
  : >"$work/gh-output"
  EVENT=$1 RELEASE=$2 FULL_MATRIX=$3 GITHUB_OUTPUT="$work/gh-output" bash -eo pipefail "$work/matrix.sh" >/dev/null
  sed -n 's/^images=//p' "$work/gh-output" | jq -r 'map(.pg) | join(" ")'
}
want_majors() {
  local got
  got=$(majors "$2" "$3" "$4" 2>&1) || true
  if [ "$got" = "$5" ]; then ok "$1"; else fail "$1: '$got', want '$5'"; fi
}
want_majors "a PR runs 17" pull_request false "" 17
want_majors "a push to main runs 17" push false "" 17
want_majors "a release run (tag push) runs 17" push true "" 17
want_majors "a re-dispatched release runs 17" workflow_dispatch true "" 17
want_majors "the schedule runs 14-18" schedule false "" "14 15 16 17 18"
want_majors "workflow_dispatch runs 14-18" workflow_dispatch false true "14 15 16 17 18"
want_majors "workflow_dispatch with full-matrix=false runs 17" workflow_dispatch false false 17
grep -qxF '      images: ${{ steps.matrix.outputs.images }}' "$work/pg-matrix.job" &&
  grep -qxF '    needs: [changes, pg-matrix]' "$work/pg-conformance.job" &&
  grep -qxF '        include: ${{ fromJSON(needs.pg-matrix.outputs.images) }}' "$work/pg-conformance.job" &&
  grep -qE '^        image: mirror\.gcr\.io/library/\$\{\{ matrix\.image \}\}( |$)' "$work/pg-conformance.job" &&
  ok "pg-conformance's service image is matrix.image (through mirror.gcr.io), from pg-matrix's output" ||
  fail "pg-conformance does not take its service image from pg-matrix (needs, matrix include, image: mirror.gcr.io/library/\${{ matrix.image }})"

# --- every service and container image in .github/workflows ------------------

# zizmor cannot see through a matrix built from a job output, so the
# pg-conformance service image carries the one zizmor ignore in the repo,
# with its reason; any other ignore would hide a finding nobody reviewed.
pg_image_line='        image: mirror.gcr.io/library/${{ matrix.image }} # zizmor: ignore[unpinned-images] pinned by digest in hack/pg-images.txt'
grep -n 'zizmor: *ignore' .github/workflows/*.yml >"$work/ignores" || true
if [ "$(wc -l <"$work/ignores" | tr -d ' ')" = 1 ] && grep -qF "${pg_image_line#        }" "$work/ignores"; then
  ok "the only zizmor ignore in .github/workflows is pg-conformance's unpinned-images, with its reason"
else
  fail "zizmor ignores in .github/workflows must be exactly pg-conformance's service image line" "$work/ignores"
fi

# One line per image: <file>:<line>: <value>, for each image: key under a
# job's services: or container:.
awk '
  FNR == 1 { svc = 0 }
  /^    (services|container):/ { svc = 1; next }
  svc && /^    [^ ]/ { svc = 0 }
  svc && /^ +image:/ { v = $0; sub(/^ +image: */, "", v); sub(/ +#.*$/, "", v); print FILENAME ":" FNR ": " v }
' .github/workflows/*.yml >"$work/images"
[ -s "$work/images" ] || fail "no service or container image found in .github/workflows (the scan is broken)"
while IFS= read -r line; do
  v=${line#*: }
  where=${line%%: *}
  case $v in
    *@sha256:*)
      [[ $v =~ @sha256:[0-9a-f]{64}$ ]] && ok "$where: $v is pinned by digest" || fail "$where: '$v' has a malformed digest" ;;
    'mirror.gcr.io/library/${{ matrix.image }}')
      [ "${where%%:*}" = "$ci" ] && grep -qxF "$pg_image_line" "$work/pg-conformance.job" &&
        ok "$where: matrix.image comes from hack/pg-images.txt, all pinned, pulled through mirror.gcr.io" ||
        fail "$where: \${{ matrix.image }} outside pg-conformance, whose matrix is not known to be pinned" ;;
    *) fail "$where: service or container image '$v' is not pinned by @sha256: (zizmor: unpinned-images)" ;;
  esac
done <"$work/images"

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "pg-images_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
