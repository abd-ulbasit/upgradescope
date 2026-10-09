#!/usr/bin/env bash
# The deployed docs site answers, and so does every link the README makes to
# it (#284). docs.yml built the site on every push to main for weeks while
# its deploy job failed, because GitHub Pages was off: every README link to
# the site, the root included, was a 404, and nothing looked at the live
# site. docs.yml now runs this after each deploy, against the URL the deploy
# reported, and weekly against the published site (mkdocs.yml's site_url).
#
# Usage: hack/docs-live-check.sh [--list] [base-url]
#
#   base-url  the site root, e.g. https://abd-ulbasit.github.io/upgradescope/
#             (default: site_url from mkdocs.yml)
#   --list    print the URLs that would be requested, one per line, and
#             request nothing
#
# It requests the site root and every URL in README.md that starts with
# base-url (a longer name such as <base-url>-other is not one), with any
# #anchor and trailing punctuation or emphasis marks dropped and duplicates
# merged. A request follows redirects and must end in HTTP 200; a failure
# (any other status, no answer, a timeout) is retried with exponentially
# growing waits, since a Pages deployment can take a moment to reach every
# edge. Every URL is judged: the script lists each one that never answered
# 200 and exits 1; it does not stop at the first. Anchors are stripped, not
# checked: the page's existence is what is proven here (mkdocs --strict
# checks the anchors the docs themselves link to).
#
# Exit: 0 every URL answered 200; 1 at least one did not; 2 the check could
# not run (no base URL, no README, no link to base-url in the README).
#
# Knobs (hack/docs-live-check_test.sh drives them): DOCS_LIVE_README
# (README.md), DOCS_LIVE_MKDOCS (mkdocs.yml), DOCS_LIVE_RETRIES (3: retries
# after the first attempt), DOCS_LIVE_BACKOFF (10: seconds before the first
# retry, doubled for each one: 10, 20, 40), DOCS_LIVE_TIMEOUT (20: seconds
# for one request, redirects included), DOCS_LIVE_PARALLEL (8: URLs in
# flight at once). Needs curl.
set -euo pipefail
cd "$(dirname "$0")/.."

readme=${DOCS_LIVE_README:-README.md}
mkdocs=${DOCS_LIVE_MKDOCS:-mkdocs.yml}
retries=${DOCS_LIVE_RETRIES:-3}
backoff=${DOCS_LIVE_BACKOFF:-10}
timeout=${DOCS_LIVE_TIMEOUT:-20}
parallel=${DOCS_LIVE_PARALLEL:-8}
usage='usage: hack/docs-live-check.sh [--list] [base-url]'

die() {
  echo "::error::docs-live-check: $*" >&2
  exit 2
}

for knob in retries backoff timeout parallel; do
  case ${!knob} in '' | *[!0-9]*) die "DOCS_LIVE_$(echo "$knob" | tr a-z A-Z) must be a whole number, got '${!knob}'" ;; esac
done
[ "$parallel" -ge 1 ] || die "DOCS_LIVE_PARALLEL must be at least 1"

list=0
base=""
have_base=0
for arg in "$@"; do
  case $arg in
    --list) list=1 ;;
    -*) die "unknown option $arg ($usage)" ;;
    *)
      [ "$have_base" = 0 ] || die "more than one base URL ($usage)"
      base=$arg
      have_base=1
      ;;
  esac
done

if [ "$have_base" = 0 ]; then
  [ -f "$mkdocs" ] || die "no base URL given and no $mkdocs to read site_url from ($usage)"
  base=$(sed -n -E 's/^site_url:[[:space:]]*([^[:space:]#]+).*/\1/p' "$mkdocs" | head -n 1 | tr -d "\"'")
  [ -n "$base" ] || die "no base URL given and $mkdocs has no site_url ($usage)"
fi

# stem is the base URL without its trailing slashes, base the root with one.
stem=$base
while [ "${stem%/}" != "$stem" ]; do stem=${stem%/}; done
case $stem in
  http://?* | https://?*) ;;
  *) die "base URL '$base' is not an http(s) URL ($usage)" ;;
esac
case $stem in
  *[[:space:]\#?]*) die "base URL '$base' holds whitespace, '?' or '#' ($usage)" ;;
esac
base=$stem/

[ -f "$readme" ] || die "no $readme"

# Every run of URL characters in the README that starts with the stem. A URL
# ends at whitespace or one of ) ] > < " ' ` | (markdown links, autolinks,
# table cells), so what is left of it is cut to the page: the #anchor, then
# sentence punctuation and emphasis marks (a bare **https://site/** keeps
# its asterisks).
re=$(printf '%s' "$stem" | sed -e 's#[][\.*^$+?(){}|]#\\&#g')
found=$(grep -oE "${re}[^][:space:])<>\"'\`|]*" "$readme" | sed -E 's/#.*$//; s/[.,;:!*]+$//' || true)

# on_site: the lines on stdin that are this site's pages, the bare stem as its
# root. (A function: bash 3.2 cannot parse a case inside $( ).)
on_site() {
  local u
  while IFS= read -r u; do
    case $u in
      "$stem") echo "$base" ;;
      "$stem"/*) echo "$u" ;;
      *) ;; # a longer name, e.g. <stem>-other: not this site
    esac
  done
}
links=$(on_site <<<"$found" | LC_ALL=C sort -u)
[ -n "$links" ] || die "$readme has no link to $base: the check would prove only the site root. Is the deployed URL the one the README links to?"
urls=$({ echo "$base"; echo "$links"; } | LC_ALL=C sort -u)

if [ "$list" = 1 ]; then
  echo "$urls"
  exit 0
fi

command -v curl >/dev/null || die "curl is required"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
echo "$urls" >"$work/urls"

# check_url <url> <out>: request the url until it answers 200 or the retries
# are spent; <out> gets "<last http code> <last curl exit> <attempts>" and
# <out>.err what curl said on its last attempt. The http code is the final
# response's, after redirects; 000 is no response at all.
check_url() {
  local url=$1 out=$2 attempt=1 delay=$backoff code rc
  while :; do
    rc=0
    code=$(curl -sS -L -o /dev/null -w '%{http_code}' --proto '=http,https' --max-redirs 10 \
      --connect-timeout "$timeout" --max-time "$timeout" -A 'upgradescope-docs-live-check' \
      "$url" 2>"$out.err") || rc=$?
    if [ "$code" = 200 ] && [ "$rc" = 0 ]; then break; fi
    [ "$attempt" -le "$retries" ] || break
    sleep "$delay"
    delay=$((delay * 2))
    attempt=$((attempt + 1))
  done
  echo "$code $rc $attempt" >"$out"
}

total=$(wc -l <"$work/urls" | tr -d ' ')
echo "docs-live-check: $total URLs under $base ($retries retries, ${backoff}s backoff doubling, ${timeout}s timeout)"
i=0
while IFS= read -r url; do
  i=$((i + 1))
  check_url "$url" "$work/r.$i" </dev/null &
  if [ $((i % parallel)) -eq 0 ]; then wait; fi
done <"$work/urls"
wait

bad=0
i=0
while IFS= read -r url; do
  i=$((i + 1))
  read -r code rc attempts <"$work/r.$i"
  if [ "$code" = 200 ] && [ "$rc" = 0 ]; then
    note=""
    [ "$attempts" = 1 ] || note="  (attempt $attempts)"
    echo "ok   200  $url$note"
    continue
  fi
  bad=$((bad + 1))
  if [ "$code" = 000 ] || [ "$rc" != 0 ]; then
    why="no answer (curl exit $rc, HTTP $code)"
  else
    why="HTTP $code"
  fi
  echo "FAIL $why  $url  (after $attempts attempts)"
  sed 's/^/       curl: /' "$work/r.$i.err"
  echo "::error::docs-live-check: $url: $why after $attempts attempts" >&2
done <"$work/urls"

if [ "$bad" != 0 ]; then
  echo "docs-live-check: $bad of $total URLs under $base did not answer 200" >&2
  exit 1
fi
echo "docs-live-check: all $total URLs under $base answered 200"
