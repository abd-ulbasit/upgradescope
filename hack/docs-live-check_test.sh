#!/usr/bin/env bash
# Offline tests for hack/docs-live-check.sh (make hack-test, #284). A stub
# site, a python3 http.server on a free loopback port, answers by path:
#
#   /site/ and /site/ok...   200            /site/status-<code>/  that code
#   /site/after-<n>/...      404 for the first n requests of that exact path,
#                            then 200 (a deployment still propagating)
#   /site/redirect-to-ok/    301 to a 200    /site/redirect-to-missing/  301 to a 404
#   /site/loop/              redirects to itself
#   /site/slow/              answers 200 after 3 s
#   /part/ok...              200, but /part/ itself is a 404 (a site root that
#                            is down while every README link is up)
#
# and a fixture README holds the links. The check must pass when every URL
# answers 200 (redirects followed, #anchors stripped, duplicates merged,
# bare/angle-bracket/reference links read, a longer site name left alone),
# list EVERY URL that fails (404, 500, a redirect to a 404, a redirect loop,
# a timeout, a refused connection) rather than stop at the first, retry with
# doubling waits until a slow deployment answers, and refuse to run (exit 2)
# when it has nothing to prove. --list reads the real README and mkdocs.yml
# with no network. Needs bash, curl and python3.
#
# The stub is built not to flake on a loaded host (#321): it listens with a
# deep backlog (http.server's default of 5 overflows when the check opens
# eight connections at once, and the kernel then resets or refuses the
# overflow), the test polls the port with a real connect before the first
# case rather than trusting a file or a sleep, and every case whose verdict
# does not depend on the first connection goes through a curl wrapper that
# retries a refused (7), empty (52) or reset (56) connection. The case that
# does depend on it (a refused connection must fail) keeps plain curl; the
# timeout, loop and status cases are not connect errors, so the wrapper
# never retries them.
set -euo pipefail
cd "$(dirname "$0")/.."

for tool in curl python3; do
  command -v "$tool" >/dev/null || { echo "docs-live-check_test: $tool is required" >&2; exit 1; }
done

work=$(mktemp -d)
srv=""
trap '[ -z "$srv" ] || { kill "$srv" && wait "$srv"; } 2>/dev/null || true; rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}

cat >"$work/stub.py" <<'EOF'
import collections, http.server, sys, threading, time

LOG, READY = sys.argv[1], sys.argv[2]
seen = collections.Counter()
lock = threading.Lock()


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, *args):
        pass

    def do_GET(self):
        path = self.path
        with lock:
            seen[path] += 1
            n = seen[path]
            with open(LOG, "a") as f:
                f.write(path + "\n")
        code, loc = 404, None
        if path == "/site/" or path.startswith("/site/ok") or path.startswith("/part/ok"):
            code = 200
        elif path.startswith("/site/status-"):
            code = int(path.split("/")[2][len("status-"):])
        elif path.startswith("/site/after-"):
            code = 404 if n <= int(path.split("/")[2][len("after-"):]) else 200
        elif path.startswith("/site/redirect-to-ok"):
            code, loc = 301, "/site/ok/"
        elif path.startswith("/site/redirect-to-missing"):
            code, loc = 301, "/site/missing/"
        elif path.startswith("/site/loop"):
            code, loc = 302, path
        elif path.startswith("/site/slow"):
            time.sleep(3)
            code = 200
        body = b"stub\n"
        try:
            self.send_response(code)
            if loc:
                self.send_header("Location", loc)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass


class S(http.server.ThreadingHTTPServer):
    # The default backlog is 5; the check opens up to DOCS_LIVE_PARALLEL
    # connections at once and a busy host accepts slowly, so the overflow
    # was reset or refused (#321).
    request_queue_size = 128


srv = S(("127.0.0.1", 0), H)
with open(READY, "w") as f:
    f.write(str(srv.server_address[1]))
srv.serve_forever()
EOF
python3 "$work/stub.py" "$work/requests" "$work/port" &
srv=$!
for _ in $(seq 1 300); do
  [ -s "$work/port" ] && break
  kill -0 "$srv" 2>/dev/null || { echo "FAIL the stub site exited before it bound a port" >&2; exit 1; }
  sleep 0.1
done
[ -s "$work/port" ] || { echo "FAIL the stub site never came up" >&2; exit 1; }
port=$(cat "$work/port")
# The port file is written once the socket listens, but poll with a real
# connect (bounded: 100 x 0.1 s) so a server that is not accepting yet fails
# here, with its own message, and not as a curl error in some later case.
up=0
for _ in $(seq 1 100); do
  if python3 -c 'import socket,sys; socket.create_connection(("127.0.0.1", int(sys.argv[1])), 2).close()' "$port" 2>/dev/null; then
    up=1
    break
  fi
  kill -0 "$srv" 2>/dev/null || { echo "FAIL the stub site died after binding port $port" >&2; exit 1; }
  sleep 0.1
done
[ "$up" = 1 ] || { echo "FAIL the stub site on port $port never accepted a connection" >&2; exit 1; }
host=http://127.0.0.1:$port
site=$host/site/

# A curl wrapper, first on PATH for every run: it retries a connection that
# was refused (7), answered with nothing (52) or reset (56), up to 5 times
# with a short wait, and passes everything else straight through (so a 404,
# a timeout, a redirect loop are still the check's to judge). Only the last
# attempt's output is shown. STUB_CURL_RETRY=0 turns it off for the case
# that expects a connect failure. It calls the real curl and sleep by
# absolute path, so the sleep stub some cases put on PATH never sees it.
real_curl=$(command -v curl)
real_sleep=$(command -v sleep)
mkdir "$work/cbin"
cat >"$work/cbin/curl" <<WRAP
#!/usr/bin/env bash
n=0
while :; do
  rc=0
  out=\$($real_curl "\$@" 2>"$work/curl.err.\$\$") || rc=\$?
  case \$rc in 7 | 52 | 56) [ "\${STUB_CURL_RETRY:-1}" = 1 ] && [ \$n -lt 5 ] || break ;; *) break ;; esac
  n=\$((n + 1))
  $real_sleep 0.\$n
done
printf '%s' "\$out"
cat "$work/curl.err.\$\$" >&2
rm -f "$work/curl.err.\$\$"
exit \$rc
WRAP
chmod +x "$work/cbin/curl"

# readme <line>...: the fixture README.
readme() { printf '%s\n' "$@" >"$work/README.md"; }
# run <name> <want-exit> [VAR=value ...] -- [script args]: the check against
# the fixture README, no waiting between attempts, no retries, unless a
# VAR=value says otherwise. Output lands in $work/out, the stub's request
# log (one path per request) in $work/requests.
run() {
  local name=$1 want=$2 got=0
  local envs=(DOCS_LIVE_README="$work/README.md" DOCS_LIVE_MKDOCS="$work/mkdocs.yml" DOCS_LIVE_BACKOFF=0 DOCS_LIVE_RETRIES=0 DOCS_LIVE_TIMEOUT=10 no_proxy=127.0.0.1 PATH="$work/cbin:$PATH")
  shift 2
  while [ "$1" != -- ]; do
    envs+=("$1")
    shift
  done
  shift
  : >"$work/requests"
  env "${envs[@]}" hack/docs-live-check.sh "$@" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ]; then ok "$name"; else
    echo "(exit $got, want $want)" >>"$work/out"
    fail "$name" "$work/out"
  fi
}
has() { if grep -qF -- "$2" "$work/out"; then ok "$1"; else fail "$1" "$work/out"; fi; }
hasnt() { if grep -qF -- "$2" "$work/out"; then fail "$1" "$work/out"; else ok "$1"; fi; }
# requests <name> <path> <count>: how often the stub saw exactly that path.
requests() {
  local got
  got=$(grep -cxF -- "$2" "$work/requests" || true)
  if [ "$got" = "$3" ]; then ok "$1"; else
    echo "$2 requested $got times, want $3" >"$work/why"
    cat "$work/requests" >>"$work/why"
    fail "$1" "$work/why"
  fi
}
# lists <name> <want>: --list printed exactly <want> (the output of run).
lists() {
  if [ "$(cat "$work/out")" = "$2" ]; then ok "$1"; else
    printf 'want:\n%s\n' "$2" >>"$work/out"
    fail "$1" "$work/out"
  fi
}

# --- reading the README -------------------------------------------------

readme \
  "[docs]($site) and [guide](${site}ok/guide/#install) or [again](${site}ok/guide/#other)." \
  "**Documentation: ${site}**" \
  "<${site}redirect-to-ok/> then ${site}ok/sentence/." \
  "[ref]: ${site}ok/ref/" \
  "| col | [table](${site}ok/table/) | \`${site}ok/code/\` |" \
  "A longer name is another site: [x]($host/site-other/page/)." \
  "The bare stem is the root: ($host/site)" \
  "Another host: https://example.com/site/ok/nope/ and ${host}x/site/ok/nope/"
run "--list reads every form of link, once" 0 -- --list "$site"
lists "--list prints the sorted set of pages, anchors and punctuation gone" "$site
${site}ok/code/
${site}ok/guide/
${site}ok/ref/
${site}ok/sentence/
${site}ok/table/
${site}redirect-to-ok/"
run "a base URL without its trailing slash is the same site" 0 -- --list "$host/site"
lists "the stem lists the same pages" "$site
${site}ok/code/
${site}ok/guide/
${site}ok/ref/
${site}ok/sentence/
${site}ok/table/
${site}redirect-to-ok/"
run "a base URL with several trailing slashes is the same site" 0 -- --list "$site//"
run "--list requests nothing" 0 -- --list "$site"
if [ -s "$work/requests" ]; then fail "--list requested a URL" "$work/requests"; else ok "--list requested nothing"; fi

# --- every URL answers 200 ---------------------------------------------

run "every linked URL answering 200 passes" 0 -- "$site"
has "the summary says all answered 200" "all 7 URLs under $site answered 200"
requests "the root is requested" /site/ 1
requests "an anchored link is requested once, without its anchor" /site/ok/guide/ 1
requests "a redirecting link is requested" /site/redirect-to-ok/ 1
requests "the redirect's target is followed" /site/ok/ 1
requests "a link under a longer site name is not requested" /site-other/page/ 0
requests "a bare stem is the root, not a second page" /site 0
if grep -q '#' "$work/requests"; then fail "an anchor reached the server" "$work/requests"; else ok "no anchor reached the server"; fi

# --- failures are all listed -------------------------------------------

readme "[a](${site}status-404/) [b](${site}ok/fine/) [c](${site}status-500/) [d](${site}redirect-to-missing/)"
run "non-200s fail the check" 1 -- "$site"
has "a 404 is named" "FAIL HTTP 404  ${site}status-404/"
has "a 500 is named" "FAIL HTTP 500  ${site}status-500/"
has "a redirect to a 404 is named by the final status" "FAIL HTTP 404  ${site}redirect-to-missing/"
has "a good link is still judged" "ok   200  ${site}ok/fine/"
has "the failure count is stated" "3 of 5 URLs under $site did not answer 200"
has "each failure is an error annotation" "::error::docs-live-check: ${site}status-500/: HTTP 500 after 1 attempts"

readme "[a](${site}loop/)"
run "a redirect loop fails" 1 -- "$site"
has "the loop is named, with curl's exit" "FAIL no answer (curl exit 47"

readme "[a](${site}slow/)"
run "a request that outlasts the timeout fails" 1 DOCS_LIVE_TIMEOUT=1 -- "$site"
has "the timeout is named, with curl's exit" "FAIL no answer (curl exit 28"
run "a slow answer within the timeout passes" 0 DOCS_LIVE_TIMEOUT=10 -- "$site"

readme "[a](http://127.0.0.1:1/site/ok/a/)"
run "a refused connection fails" 1 STUB_CURL_RETRY=0 -- http://127.0.0.1:1/site/
has "the refusal is named, with curl's exit" "FAIL no answer (curl exit 7"
has "curl's own message is shown" "curl: curl: (7)"

readme "[a]($host/part/ok/a/)"
run "a site root that is down fails though every README link is up" 1 -- "$host/part/"
has "the root is the failure" "FAIL HTTP 404  $host/part/"
has "the README link is still judged" "ok   200  $host/part/ok/a/"

# --- propagation: retries with doubling waits ---------------------------

readme "[a](${site}after-2/a/)"
run "a link that answers 200 on the third try passes with 3 retries" 0 DOCS_LIVE_RETRIES=3 -- "$site"
requests "it was requested three times" /site/after-2/a/ 3
has "the pass says which attempt" "ok   200  ${site}after-2/a/  (attempt 3)"

readme "[a](${site}after-2/b/)"
run "a link still 404 after the retries fails" 1 DOCS_LIVE_RETRIES=1 -- "$site"
requests "it was requested the first time and once more" /site/after-2/b/ 2
has "the failure counts the attempts" "(after 2 attempts)"

readme "[a](${site}after-2/c/)"
run "no retries means one attempt" 1 DOCS_LIVE_RETRIES=0 -- "$site"
requests "it was requested once" /site/after-2/c/ 1

mkdir "$work/bin"
cat >"$work/bin/sleep" <<'EOF'
#!/usr/bin/env bash
echo "$1" >>"$SLEEP_LOG"
EOF
chmod +x "$work/bin/sleep"
readme "[a](${site}after-9/d/)"
: >"$work/sleeps"
run "a link that never answers 200 fails after every retry" 1 PATH="$work/bin:$work/cbin:$PATH" SLEEP_LOG="$work/sleeps" DOCS_LIVE_RETRIES=3 DOCS_LIVE_BACKOFF=10 -- "$site"
requests "it was requested four times" /site/after-9/d/ 4
if [ "$(tr '\n' ' ' <"$work/sleeps")" = "10 20 40 " ]; then ok "the waits between attempts double: 10, 20, 40"; else fail "the waits between attempts double" "$work/sleeps"; fi
readme "[a](${site}ok/e/)"
: >"$work/sleeps"
run "a first-try 200 never waits" 0 PATH="$work/bin:$work/cbin:$PATH" SLEEP_LOG="$work/sleeps" DOCS_LIVE_BACKOFF=10 -- "$site"
if [ -s "$work/sleeps" ]; then fail "a first-try 200 waited" "$work/sleeps"; else ok "a first-try 200 did not wait"; fi

# more URLs than DOCS_LIVE_PARALLEL are still all judged
{
  for i in 1 2 3 4 5 6 7; do echo "[l$i](${site}ok/p$i/)"; done
  echo "[bad](${site}status-404/p/)"
} >"$work/README.md"
run "more URLs than run at once are all judged" 1 DOCS_LIVE_PARALLEL=3 -- "$site"
has "the last batch ran" "ok   200  ${site}ok/p7/"
has "the failure among them is listed" "FAIL HTTP 404  ${site}status-404/p/"
has "all nine were judged" "1 of 9 URLs"

# --- the base URL defaults to mkdocs.yml's site_url ---------------------

readme "[a](${site}ok/a/)"
printf 'site_name: x\nsite_url: %s # the root\nnav: []\n' "$site" >"$work/mkdocs.yml"
run "no argument reads site_url from mkdocs.yml" 0 --
has "it checked that site" "all 2 URLs under $site answered 200"
printf 'site_url: "%s"\n' "$site" >"$work/mkdocs.yml"
run "a quoted site_url is read" 0 --
printf 'site_name: x\n' >"$work/mkdocs.yml"
run "no site_url and no argument cannot run" 2 --
has "it says so" "has no site_url"
rm "$work/mkdocs.yml"
run "no mkdocs.yml and no argument cannot run" 2 --

# --- nothing to prove: exit 2, never a pass -----------------------------

readme "[a](https://example.com/)" "[b]($host/site-other/x/)"
run "a README with no link to the site cannot pass" 2 -- "$site"
has "it says so" "has no link to $site"
rm "$work/README.md"
run "no README cannot pass" 2 -- "$site"
has "it names the missing file" "no $work/README.md"
readme "[a](${site}ok/a/)"
run "an empty base URL is refused" 2 -- ""
run "a non-http base URL is refused" 2 -- "ftp://example.com/x/"
run "a base URL with a query is refused" 2 -- "${site}?a=b"
run "two base URLs are refused" 2 -- "$site" "$site"
run "an unknown option is refused" 2 -- --nope "$site"
run "a non-numeric knob is refused" 2 DOCS_LIVE_RETRIES=lots -- "$site"
has "it names the knob" "DOCS_LIVE_RETRIES must be a whole number"
run "zero parallelism is refused" 2 DOCS_LIVE_PARALLEL=0 -- "$site"

# --- the real README and mkdocs.yml, offline ----------------------------

real=https://abd-ulbasit.github.io/upgradescope/
env -u DOCS_LIVE_README -u DOCS_LIVE_MKDOCS hack/docs-live-check.sh --list >"$work/real" 2>&1 || { fail "--list on the real README and mkdocs.yml" "$work/real"; }
for page in "" comparison/ getting-started/cli/ operations/scale/ operations/install/ reference/api/; do
  if grep -qxF "$real$page" "$work/real"; then ok "the real README yields $real$page"; else fail "the real README does not yield $real$page" "$work/real"; fi
done
if grep -vq '^https://abd-ulbasit.github.io/upgradescope/[^#*[:space:]]*$' "$work/real"; then
  fail "a URL the real README yields is malformed (an anchor, a * or a space)" "$work/real"
else ok "every URL the real README yields is a clean page URL"; fi
if [ "$(sort -u "$work/real" | wc -l)" = "$(wc -l <"$work/real")" ]; then ok "the real README's URLs are unique"; else fail "duplicate URLs" "$work/real"; fi
if [ "$(grep -c . "$work/real")" -ge 15 ]; then ok "the real README links at least 15 pages ($(grep -c . "$work/real"))"; else fail "the real README yields fewer than 15 pages" "$work/real"; fi

echo
if grep -q '^FAIL' "$work/results"; then
  echo "docs-live-check_test: $(grep -c '^FAIL' "$work/results") failed" >&2
  exit 1
fi
echo "docs-live-check_test: all $(grep -c '^ok' "$work/results") cases passed"
