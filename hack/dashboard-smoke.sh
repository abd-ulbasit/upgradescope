#!/usr/bin/env bash
# Proves a built binary serves its embedded dashboard: `serve` on a free
# loopback port, then GET / must be 200 text/html, and every /assets/ file
# that index.html references must be 200 with a JS or CSS content type.
# A 200 index.html alone is a blank page when the bundle it loads 404s
# (#128, DB-07). Run by CI's build job on bin/upgradescope (the real embed
# and Server routing) and by hack/release-check.sh on the release binary.
#
# Fails, rather than probing something else, when the port already answers
# before serve starts, or when serve exits early.
#
# Usage: hack/dashboard-smoke.sh <upgradescope binary>
# Knob: DASHBOARD_SMOKE_PORT (default: a random port in 30000-49999).
set -euo pipefail

bin=${1:?usage: hack/dashboard-smoke.sh <upgradescope binary>}
die() { echo "::error::dashboard-smoke: $*" >&2; exit 1; }
command -v curl >/dev/null || die "curl is required"
[ -x "$bin" ] || die "$bin is not an executable"

port=${DASHBOARD_SMOKE_PORT:-$(((RANDOM % 20000) + 30000))}
base="http://127.0.0.1:$port"
# curl exit 7: nothing listens there. Anything else (a response, a reset)
# means another process holds the port and would be probed instead.
rc=0
curl -sS -o /dev/null --max-time 2 "$base/" 2>/dev/null || rc=$?
[ "$rc" = 7 ] || die "127.0.0.1:$port already answers (curl exit $rc); pick another with DASHBOARD_SMOKE_PORT"

tmp=$(mktemp -d)
"$bin" serve --listen "127.0.0.1:$port" --ingest-token smoke --db "$tmp/smoke.sqlite" >"$tmp/serve.log" 2>&1 &
pid=$!
# Also on failure: a live server would hold the job (or the terminal) open.
trap '{ kill "$pid" && wait "$pid"; } 2>/dev/null || true; rm -rf "$tmp"' EXIT

alive() { kill -0 "$pid" 2>/dev/null || { sed 's/^/  serve: /' "$tmp/serve.log" >&2; die "serve exited ($1)"; }; }
up=""
for _ in $(seq 1 50); do
  alive "before answering /healthz"
  if curl -fsS -o /dev/null "$base/healthz" 2>/dev/null; then up=1; break; fi
  sleep 0.2
done
[ -n "$up" ] || die "serve did not answer /healthz within 10s"

got=$(curl -sS -o "$tmp/index.html" -w '%{http_code} %{content_type}' "$base/")
case "$got" in
  '200 text/html'*) echo "ok: GET / -> $got" ;;
  *) die "GET / -> '$got', want 200 text/html (dashboard missing from the binary)" ;;
esac

assets=$(grep -oE '(src|href)="/assets/[^"]+"' "$tmp/index.html" | sed -E 's/^(src|href)="//; s/"$//' | sort -u || true)
[ -n "$assets" ] || die "index.html references no /assets/ file; the bundle is not the Vite build"
for a in $assets; do
  got=$(curl -sS -o /dev/null -w '%{http_code} %{content_type}' "$base$a")
  case "$a" in
    *.js) want='200 text/javascript' ;;
    *.css) want='200 text/css' ;;
    *) want='200 ' ;;
  esac
  case "$got" in
    "$want"*) echo "ok: GET $a -> $got" ;;
    *) die "GET $a -> '$got', want '$want...' (index.html references it; the dashboard would render blank)" ;;
  esac
done
alive "while serving the dashboard; another process answered"
echo "dashboard-smoke: OK ($(echo "$assets" | wc -l | tr -d ' ') assets)"
