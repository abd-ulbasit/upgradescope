#!/usr/bin/env bash
# Asserts that an upgradescope binary serves the embedded dashboard:
# `serve` answers GET / with 200 text/html, and every /assets/ file that
# page references answers 200 with a non-empty body. A binary built without
# the bundle, or with a stale index.html naming assets it does not embed,
# fails here instead of showing users a blank page.
#
# Usage: hack/serve-smoke.sh <binary> [port]
# Used by hack/release-check.sh (the release archive's binary) and
# hack/go-install-check.sh (a `go install` binary). Needs curl.
set -euo pipefail

bin=${1:?usage: $0 <binary> [port]}
port=${2:-18430}
die() { echo "::error::serve-smoke: $*" >&2; exit 1; }
[ -x "$bin" ] || die "$bin is not an executable"

! curl -s -o /dev/null "http://127.0.0.1:$port/" 2>/dev/null \
  || die "something already listens on 127.0.0.1:$port; pass a free port as the second argument"
db=$(mktemp -d)
"$bin" serve --listen "127.0.0.1:$port" --ingest-token smoke --db "$db/smoke.sqlite" &
pid=$!
# Also on failure: a live server would hold the job (or the terminal) open.
trap 'kill "$pid" 2>/dev/null || true; rm -rf "$db"' EXIT
base="http://127.0.0.1:$port"
up=0
for _ in $(seq 1 50); do
  kill -0 "$pid" 2>/dev/null || die "serve exited (is port $port taken? pass another as the second argument)"
  curl -fsS -o /dev/null "$base/healthz" 2>/dev/null && { up=1; break; }
  sleep 0.2
done
[ "$up" = 1 ] || die "serve did not answer /healthz on port $port within 10s"
# Still ours: a server that lost the port race would answer from elsewhere.
kill -0 "$pid" 2>/dev/null || die "serve exited (is port $port taken? pass another as the second argument)"

got="$(curl -sS -o "$db/index.html" -w '%{http_code} %{content_type}' "$base/")"
case "$got" in
  '200 text/html'*) echo "ok: GET / -> $got" ;;
  *) die "GET / returned '$got', want 200 text/html (dashboard missing from the binary)" ;;
esac

assets="$(grep -oE '/assets/[^"'"'"' >]+' "$db/index.html" | sort -u || true)"
[ -n "$assets" ] || die "index.html references no /assets/ files; is it the built dashboard?"
for a in $assets; do
  got="$(curl -sS -o "$db/asset" -w '%{http_code}' "$base$a")"
  [ "$got" = 200 ] && [ -s "$db/asset" ] || die "GET $a returned $got (index.html references an asset the binary does not embed)"
  echo "ok: GET $a -> 200"
done
