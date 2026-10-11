#!/usr/bin/env bash
# The claims suite's server checks (make claims-server; CI's repo-checks
# job): two ledger claims about `serve` that a Go test proves in process,
# proven again on the built binary over HTTP. No cluster and no Docker:
# `serve` on a free loopback port with a temporary SQLite database and
# fixed test tokens (shared ingest, read, admin, and per-cluster ingest
# tokens minted with `upgradescope tokens create`, as the fleet guide
# does), driven with curl and the `clusters` subcommands.
#
#   SV-24 (#368): a name first registered by a push with no clusterId is
#     bound by the first clusterId pushed afterwards (202, 202, the name's
#     history 75 then 100); then another clusterId, or none, is a 409. The
#     server logs exactly one "cluster name adopted its first clusterId"
#     line, naming the cluster and the clusterId, and no token anywhere.
#   SE-02 (#369): a push whose cluster is renamed or deleted while its body
#     is still uploading. With a per-cluster token it answers 409 "renamed
#     or deleted" and stores nothing; with the shared ingest token it
#     registers the name like a fresh push (202), as docs/operations.md
#     documents (after a rename, a second cluster with the same UID).
#
# The SE-02 race is ordered, not timed. The push is throttled (curl
# --limit-rate) and sent with Expect: 100-continue, and its body comes
# through a pipe that is held open: the server answers 100 Continue only
# when the handler first reads the body, which is after it authenticated
# the push, and it looks the cluster up only after the whole body is read.
# The admin command runs after the 100 Continue and before the last bytes
# are written, so it lands between authentication and the lookup on every
# run, on any machine.
#
# Usage: hack/claims-server.sh [upgradescope binary]
#   (default: builds ./cmd/upgradescope into a temporary directory)
# Needs curl, jq and mkfifo.
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C # ${#s} counts bytes: the Content-Length of the held push

die() { echo "::error::claims-server: $*" >&2; exit 1; }
for c in curl jq mkfifo; do command -v "$c" >/dev/null || die "$c is required"; done

tmp=$(mktemp -d)
pid=""
cleanup() {
  if [ -n "$pid" ]; then { kill "$pid" && wait "$pid"; } 2>/dev/null || true; fi
  rm -rf "$tmp"
}
trap cleanup EXIT

bin=${1:-}
if [ -z "$bin" ]; then
  bin=$tmp/upgradescope
  go build -o "$bin" ./cmd/upgradescope
fi
[ -x "$bin" ] || die "$bin is not an executable"

checks=0
ok() { echo "ok: $*"; checks=$((checks + 1)); }

# Tokens: random per run, so "no token in the log" means something. They
# reach serve and the clusters subcommands only through files.
rand() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n'; }
ingest=claims-ingest-$(rand) read=claims-read-$(rand) admin=claims-admin-$(rand)
printf '%s\n' "$ingest" >"$tmp/ingest"
printf '%s\n' "$read" >"$tmp/read"
printf '%s\n' "$admin" >"$tmp/admin"
db=$tmp/claims.sqlite

"$bin" serve --listen 127.0.0.1:0 --db "$db" \
  --ingest-token-file "$tmp/ingest" --read-token-file "$tmp/read" --admin-token-file "$tmp/admin" \
  >"$tmp/serve.log" 2>&1 &
pid=$!
alive() { kill -0 "$pid" 2>/dev/null || { sed 's/^/  serve: /' "$tmp/serve.log" >&2; die "serve exited ($1)"; }; }
base=""
for _ in $(seq 1 100); do
  alive "before listening"
  base=$(sed -n 's#^.*server: listening on \(http://127\.0\.0\.1:[0-9][0-9]*\)$#\1#p' "$tmp/serve.log" | head -1)
  [ -z "$base" ] || break
  sleep 0.1
done
[ -n "$base" ] || die "serve did not log its listen address within 10s"
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "$base/healthz" 2>/dev/null && break
  alive "before answering /healthz"
  sleep 0.1
done
echo "claims-server: serve at $base (database $db)"

# inventory <clusterId> [psp]: a complete collection of a v1.34.2 cluster,
# clean (score 100) or with one PodSecurityPolicy, removed in 1.25 (one
# blocker at the default target: 75).
inventory() {
  jq -nc --arg uid "$1" --arg psp "${2:-}" '{
    schemaVersion: 1, clusterId: $uid, collectorSchema: 1,
    collectedAt: "2026-06-10T11:00:00Z", serverVersion: "v1.34.2",
    capabilities: (["api-usage", "deprecated-calls", "helm", "addons", "versions", "crds", "volumes"]
      | map({key: ., value: {available: true}}) | from_entries)
  } + (if $psp == "" then {} else {apiUsage: [{group: "policy", version: "v1beta1",
    kind: "PodSecurityPolicy", count: 2, namespaces: {"": 2}}]} end)'
}
# auth <token>: a curl header file carrying the token, so it is not on a
# command line.
auth() { printf 'Authorization: Bearer %s\n' "$1" >"$tmp/auth"; echo "$tmp/auth"; }
# push <token> <name> <clusterId> [psp]: POST a snapshot; prints "<status> <body>".
push() {
  local code hdr
  hdr=$(auth "$1")
  code=$(jq -nc --arg name "$2" --argjson inv "$(inventory "$3" "${4:-}")" \
    '{schemaVersion: 1, clusterName: $name, inventory: $inv}' |
    curl -sS -o "$tmp/resp" -w '%{http_code}' -H @"$hdr" \
      -H 'Content-Type: application/json' --data-binary @- "$base/api/v1/snapshots")
  echo "$code $(cat "$tmp/resp")"
}
# expect_push <want-status> <what> <push args...>
expect_push() {
  local want=$1 what=$2 got
  shift 2
  got=$(push "$@")
  [ "${got%% *}" = "$want" ] || die "$what: got $got, want $want"
  ok "$what: $want"
}
api() { curl -fsS -H @"$(auth "$read")" "$base$1"; }
# cluster_json <name>: the API's record of the cluster, or nothing.
cluster_json() { api /api/v1/clusters | jq -c --arg n "$1" '.[] | select(.name == $n)'; }
# listed: `clusters list --server-url` as "<id> <name> <uid>" lines.
listed() {
  "$bin" clusters list --server-url "$base" --read-token-file "$tmp/read" >"$tmp/list" ||
    die "clusters list failed: $(cat "$tmp/list")"
  awk 'NR > 1 { print $1, $2, $3 }' "$tmp/list"
}
list_has() { listed | awk -v n="$1" -v u="${2:-}" '$2 == n && (u == "" || $3 == u) { f = 1 } END { exit !f }'; }

# ---- SV-24 (#368): the reverse-order adoption -------------------------------
uid1=abababab-abab-abab-abab-abababababab
uid2=cdcdcdcd-cdcd-cdcd-cdcd-cdcdcdcdcdcd
expect_push 202 "SV-24: a push with clusterId \"\" registers sv24" "$ingest" sv24 "" psp
expect_push 202 "SV-24: the first push with a clusterId is accepted" "$ingest" sv24 "$uid1"
c=$(cluster_json sv24)
[ "$(jq -r .clusterUid <<<"$c")" = "$uid1" ] || die "SV-24: sv24 is $c, want it bound to $uid1"
ok "SV-24: the name is now bound to $uid1"
id=$(jq -r .id <<<"$c") target=$(jq -r .latest.target <<<"$c")
scores=$(api "/api/v1/clusters/$id/history?target=$target" | jq -c 'map(.score)')
[ "$scores" = "[75,100]" ] || die "SV-24: history at $target is $scores, want [75,100]: the adopting push continues the name's history"
ok "SV-24: the history holds two points, $scores at $target"
expect_push 409 "SV-24: then a push with another clusterId" "$ingest" sv24 "$uid2" psp
expect_push 409 "SV-24: and a push with none" "$ingest" sv24 "" psp
n=$(grep -c 'cluster name adopted its first clusterId' "$tmp/serve.log" || true)
[ "$n" = 1 ] || die "SV-24: the server logged $n adoption lines, want 1: $(cat "$tmp/serve.log")"
grep 'cluster name adopted its first clusterId' "$tmp/serve.log" | grep -qF "cluster=\"sv24\" clusterId=\"$uid1\"" ||
  die "SV-24: the adoption line does not name the cluster and the clusterId: $(grep adopted "$tmp/serve.log")"
ok "SV-24: one adoption line, naming sv24 and $uid1"

# ---- SE-02 (#369): a rename or delete while the push uploads ----------------
# The padding is JSON whitespace: at 1 KiB/s the throttled body takes a few
# seconds, and the pipe holds its last bytes until the admin command returns.
pad=$(printf '%3072s' '')
curl_log() { grep -viE '^> authorization' "$tmp/curl.err" || true; } # curl -v, without the token
# race <token> <name> <clusterId> <clusters subcommand and args...>: prints
# the push's "<status> <body>" once the admin command landed mid-upload.
race() {
  local tok=$1 name=$2 uid=$3 pre rest len cpid wpid code i hdr
  shift 3
  hdr=$(auth "$tok")
  pre=$(printf '{"schemaVersion":1,"clusterName":"%s",%s' "$name" "$pad")
  rest=$(printf '"inventory":%s}' "$(inventory "$uid")")
  len=$((${#pre} + ${#rest}))
  rm -f "$tmp/body" "$tmp/go"
  mkfifo "$tmp/body"
  { printf '%s' "$pre"
    for i in $(seq 1 600); do [ -e "$tmp/go" ] && break; sleep 0.1; done
    printf '%s' "$rest"; } >"$tmp/body" &
  wpid=$!
  # -T - streams the pipe; a Content-Length (and no chunked encoding) sends
  # it the way an agent sends a push.
  curl -sS -v --limit-rate 1k -X POST -T - -H 'Transfer-Encoding:' -H "Content-Length: $len" \
    -H 'Expect: 100-continue' -H 'Content-Type: application/json' -H @"$hdr" \
    -o "$tmp/resp" -w '%{http_code}' "$base/api/v1/snapshots" <"$tmp/body" >"$tmp/code" 2>"$tmp/curl.err" &
  cpid=$!
  for i in $(seq 1 100); do
    grep -q '^< HTTP/1.1 100 Continue' "$tmp/curl.err" && break
    kill -0 "$cpid" 2>/dev/null || break
    sleep 0.1
  done
  grep -q '^< HTTP/1.1 100 Continue' "$tmp/curl.err" || {
    touch "$tmp/go"; wait "$cpid" || true; wait "$wpid" || true
    die "race: the server never answered 100 Continue (it did not authenticate the push and start reading it): $(curl_log | tail -5) $(cat "$tmp/resp" 2>/dev/null)"
  }
  "$bin" clusters "$@" --server-url "$base" --admin-token-file "$tmp/admin" >"$tmp/admin.out" 2>&1 ||
    { touch "$tmp/go"; die "clusters $*: $(cat "$tmp/admin.out")"; }
  kill -0 "$cpid" 2>/dev/null || { touch "$tmp/go"; die "race: the push finished before clusters $* returned: $(cat "$tmp/code" "$tmp/resp")"; }
  touch "$tmp/go"
  wait "$wpid" || true
  wait "$cpid" || die "race: curl failed: $(curl_log | tail -5)"
  grep -qiE '^> Transfer-Encoding' "$tmp/curl.err" && die "race: curl sent the push chunked"
  code=$(cat "$tmp/code")
  echo "$code $(cat "$tmp/resp")"
}

# Per-cluster tokens: a rename, then a delete, mid-upload.
pc_rename=$("$bin" tokens create se02-pc --db "$db" 2>/dev/null)
pc_delete=$("$bin" tokens create se02-pd --db "$db" 2>/dev/null)
expect_push 202 "SE-02: se02-pc registers with its per-cluster token" "$pc_rename" se02-pc 11111111-1111-1111-1111-111111111111
expect_push 202 "SE-02: se02-pd registers with its per-cluster token" "$pc_delete" se02-pd 22222222-2222-2222-2222-222222222222
first=$(jq -r .id <<<"$(cluster_json se02-pc)")

got=$(race "$pc_rename" se02-pc 11111111-1111-1111-1111-111111111111 rename se02-pc se02-pc-renamed)
[ "${got%% *}" = 409 ] && grep -qF 'renamed or deleted' <<<"$got" ||
  die "SE-02: a per-cluster-token push racing a rename: got $got, want 409 \"renamed or deleted\""
ok "SE-02: a per-cluster-token push racing a rename mid-upload: 409 renamed or deleted"
list_has se02-pc && die "SE-02: clusters list shows se02-pc again after the refused push: $(listed)"
list_has se02-pc-renamed 11111111-1111-1111-1111-111111111111 || die "SE-02: clusters list has no se02-pc-renamed: $(listed)"
n=$(api "/api/v1/clusters/$first/history" | jq length)
[ "$n" = 1 ] || die "SE-02: se02-pc-renamed's history has $n points after the refused push, want 1 (nothing stored)"
ok "SE-02: clusters list shows only se02-pc-renamed, and its history is unchanged: nothing stored"

got=$(race "$pc_delete" se02-pd 22222222-2222-2222-2222-222222222222 delete se02-pd)
[ "${got%% *}" = 409 ] && grep -qF 'renamed or deleted' <<<"$got" ||
  die "SE-02: a per-cluster-token push racing a delete: got $got, want 409 \"renamed or deleted\""
ok "SE-02: a per-cluster-token push racing a delete mid-upload: 409 renamed or deleted"
list_has se02-pd && die "SE-02: clusters list shows se02-pd after the refused push: $(listed)"
ok "SE-02: clusters list has no se02-pd: the deleted cluster was not registered again"

# The shared ingest token names its cluster only in the body: the documented
# behaviour is a fresh registration.
uid3=33333333-3333-3333-3333-333333333333 uid4=44444444-4444-4444-4444-444444444444
expect_push 202 "SE-02: se02-sd registers with the shared token" "$ingest" se02-sd "$uid3"
expect_push 202 "SE-02: se02-sr registers with the shared token" "$ingest" se02-sr "$uid4"
sd_first=$(jq -r .id <<<"$(cluster_json se02-sd)")
sr_first=$(jq -r .id <<<"$(cluster_json se02-sr)")

got=$(race "$ingest" se02-sd "$uid3" delete se02-sd)
[ "${got%% *}" = 202 ] || die "SE-02: a shared-token push racing a delete: got $got, want 202 (documented: it registers the name like a fresh push)"
ok "SE-02: a shared-token push racing a delete mid-upload: 202"
sd_now=$(jq -r .id <<<"$(cluster_json se02-sd)")
list_has se02-sd "$uid3" && [ -n "$sd_now" ] && [ "$sd_now" != "$sd_first" ] ||
  die "SE-02: se02-sd after the push is id '$sd_now' (was $sd_first), want it registered again as a new cluster: $(listed)"
ok "SE-02: clusters list shows se02-sd registered again (id $sd_first, now $sd_now)"

got=$(race "$ingest" se02-sr "$uid4" rename se02-sr se02-sr-renamed)
[ "${got%% *}" = 202 ] || die "SE-02: a shared-token push racing a rename: got $got, want 202 (documented: it registers the old name like a fresh push)"
ok "SE-02: a shared-token push racing a rename mid-upload: 202"
sr_now=$(jq -r .id <<<"$(cluster_json se02-sr)")
list_has se02-sr "$uid4" && list_has se02-sr-renamed "$uid4" && [ "$sr_now" != "$sr_first" ] ||
  die "SE-02: want se02-sr (a new cluster) and se02-sr-renamed (id $sr_first) both with UID $uid4: $(listed)"
ok "SE-02: clusters list shows se02-sr-renamed and a new se02-sr, both with UID $uid4"

# ---- the log ---------------------------------------------------------------
alive "at the end of the checks"
for t in "$ingest" "$read" "$admin" "$pc_rename" "$pc_delete"; do
  ! grep -qF -- "$t" "$tmp/serve.log" || die "the server log carries a token (prefix ${t:0:8})"
done
ok "the server log carries no token"
echo "claims-server: OK ($checks checks)"
