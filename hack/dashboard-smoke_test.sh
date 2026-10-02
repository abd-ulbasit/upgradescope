#!/usr/bin/env bash
# Tests for hack/dashboard-smoke.sh (make hack-test) against a stub
# `upgradescope serve`: a tiny Go file server over a fixture bundle, built
# here. The smoke check must fail on each way the old release-check step
# went green on a broken binary (#128, DB-07): a referenced asset missing
# (blank dashboard behind a 200 index.html), the port already held by
# another process, and serve exiting early. Offline; needs Go and curl.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
held=""
trap '[ -z "$held" ] || { kill "$held" && wait "$held"; } 2>/dev/null || true; rm -rf "$work"' EXIT

mkdir -p "$work/stub" "$work/dist/assets"
cat >"$work/stub/main.go" <<'EOF'
// A stand-in for `upgradescope serve --listen ADDR ...`: serves $STUB_DIST
// like the embedded dashboard, /healthz like the server, or exits at once
// when STUB_EXIT is set. STUB_JS_TYPE overrides the .js Content-Type, as a
// host's /etc/mime.types can for Go's mime table.
package main

import (
	"net/http"
	"os"
	"strings"
)

func main() {
	if os.Getenv("STUB_EXIT") != "" {
		os.Exit(1)
	}
	addr := ""
	for i, a := range os.Args {
		if a == "--listen" && i+1 < len(os.Args) {
			addr = os.Args[i+1]
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	files := http.FileServer(http.Dir(os.Getenv("STUB_DIST")))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if t := os.Getenv("STUB_JS_TYPE"); t != "" && strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", t)
		}
		files.ServeHTTP(w, r)
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		os.Exit(1)
	}
}
EOF
printf 'module example.com/stub\n\ngo 1.22\n' >"$work/stub/go.mod"
(cd "$work/stub" && GOPROXY=off GOWORK=off go build -o "$work/upgradescope" .)

cat >"$work/dist/index.html" <<'EOF'
<!doctype html>
<html><head>
<script type="module" crossorigin src="/assets/index-abc123.js"></script>
<link rel="stylesheet" crossorigin href="/assets/index-def456.css">
</head><body><div id="root"></div></body></html>
EOF
echo 'console.log("dashboard")' >"$work/dist/assets/index-abc123.js"
echo 'body { margin: 0 }' >"$work/dist/assets/index-def456.css"

port() { echo $(((RANDOM % 20000) + 30000)); }

: >"$work/results"
# expect <name> <want-exit> <needle> [env...]: smoke-test the stub.
expect() {
  local name=$1 want=$2 needle=$3 got=0
  shift 3
  env STUB_DIST="$work/dist" "$@" hack/dashboard-smoke.sh "$work/upgradescope" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

expect "a good bundle passes" 0 "ok: GET /assets/index-def456.css -> 200 text/css"
expect "every referenced asset is fetched" 0 "ok: GET /assets/index-abc123.js -> 200 text/javascript"

# Both registered JavaScript types load a module script; anything else
# (text/plain from a bare mime table) is blocked by the browser.
expect "application/javascript passes" 0 "ok: GET /assets/index-abc123.js -> 200 application/javascript" \
  STUB_JS_TYPE="application/javascript; charset=utf-8"
expect "a .js served as text/plain fails" 1 "GET /assets/index-abc123.js -> '200 text/plain" STUB_JS_TYPE=text/plain

mv "$work/dist/assets/index-def456.css" "$work/css.bak"
expect "a missing referenced asset fails" 1 "GET /assets/index-def456.css -> '404"
mv "$work/css.bak" "$work/dist/assets/index-def456.css"

cp "$work/dist/index.html" "$work/index.bak"
echo '<!doctype html><html><body>no bundle</body></html>' >"$work/dist/index.html"
expect "an index.html that references no asset fails" 1 "references no /assets/ file"
mv "$work/index.bak" "$work/dist/index.html"

expect "serve exiting early fails" 1 "serve exited" STUB_EXIT=1

# Another process already answering on the port: the old check probed it
# instead of the binary under test, and passed.
p=$(port)
STUB_DIST="$work/dist" "$work/upgradescope" serve --listen "127.0.0.1:$p" &
held=$!
for _ in $(seq 1 50); do curl -fsS -o /dev/null "http://127.0.0.1:$p/healthz" 2>/dev/null && break; sleep 0.1; done
expect "a port already in use fails" 1 "127.0.0.1:$p already answers" DASHBOARD_SMOKE_PORT="$p"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "dashboard-smoke_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
