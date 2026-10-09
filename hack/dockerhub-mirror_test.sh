#!/usr/bin/env bash
# Docker Hub rate-limits anonymous pulls per source IP (429
# toomanyrequests), and a hosted runner shares its IP with many others, so a
# CI job that pulls from Docker Hub fails now and then with no commit to
# blame. CI pulls through Google's public pull-through cache instead,
# mirror.gcr.io (same digests, no credentials; the daemon falls back to
# docker.io for an image the mirror lacks). make hack-test checks:
#   - .github/actions/dockerhub-mirror: its script merges into an existing
#     daemon.json (other keys and mirrors kept, ours first and never twice, a
#     second run changes nothing), starts from no file or an empty one,
#     refuses invalid JSON without touching the file, and fails if docker does
#     not restart or does not report the mirror;
#   - every job of every workflow that builds an image, creates a kind
#     cluster or otherwise pulls a container image has that step before its
#     first such step (so a new job that forgets it fails here, not on a busy
#     day), and none that has services: or container: has it (they start
#     before the steps and the daemon restart would kill them);
#   - every docker/setup-buildx-action step points its BuildKit at the mirror
#     too (a docker-container builder ignores the daemon's mirrors), and
#     pulls its builder image by one digest, the same in every workflow (the
#     mirror serves a digest from its cache; the first pull of the moving
#     buildx-stable-1 tag in a job went to Docker Hub and timed out);
#   - every services: or container: image that Docker Hub would serve is
#     named mirror.gcr.io/... instead;
#   - CONTRIBUTING.md documents it.
# Each rule is also run against a mutated or extended workflow that must fail
# it, and against jobs that must not. Offline; needs jq, perl.
set -euo pipefail
cd "$(dirname "$0")/.."

action=.github/actions/dockerhub-mirror/action.yml
ci=.github/workflows/ci.yml
release=.github/workflows/release.yml
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: >"$work/results"
ok() { echo "ok   $1" | tee -a "$work/results"; }
fail() {
  echo "FAIL $1" >&2
  [ -z "${2:-}" ] || sed 's/^/     /' "$2" >&2
  echo "FAIL $1" >>"$work/results"
}

# --- the action's script -----------------------------------------------------

grep -qxF "      if: runner.os == 'Linux'" "$action" &&
  ok "the action's step runs on Linux runners only" || fail "the action's step is not limited to runner.os == 'Linux'"
awk '/^      run: \|$/ { r = 1; next } r { if ($0 !~ /^ *$/ && $0 !~ /^        /) exit; print substr($0, 9) }' "$action" >"$work/mirror.sh"
[ -s "$work/mirror.sh" ] || { echo "FAIL no run block in $action" >&2; exit 1; }

# The runner's commands, stubbed: sudo runs its arguments, systemctl and
# docker only log (and fail on request), docker info reads the mirrors from
# the daemon.json the script wrote, sleep returns at once.
mkdir -p "$work/bin"
printf '#!/bin/sh\nexec "$@"\n' >"$work/bin/sudo"
printf '#!/bin/sh\nexit 0\n' >"$work/bin/sleep"
cat >"$work/bin/systemctl" <<'EOF'
#!/bin/sh
echo "systemctl $*" >>"$STUB_LOG"
[ "${STUB_SYSTEMCTL_FAIL:-}" != 1 ]
EOF
cat >"$work/bin/docker" <<'EOF'
#!/bin/sh
echo "docker $*" >>"$STUB_LOG"
[ "${STUB_DOCKER_DOWN:-}" != 1 ] || exit 1
case "$*" in
  *--format*)
    if [ "${STUB_NO_MIRROR:-}" = 1 ]; then echo null; else jq -c '[(.["registry-mirrors"] // [])[] | . + "/"]' "$DOCKER_DAEMON_JSON"; fi ;;
  *) echo "Server Version: stub" ;;
esac
EOF
chmod +x "$work/bin/"*

# run_action <config file>: the script as the step runs it; output and exit
# status in $work/out and $?.
run_action() {
  : >"$work/stub.log"
  PATH="$work/bin:$PATH" DOCKER_DAEMON_JSON=$1 STUB_LOG="$work/stub.log" bash "$work/mirror.sh" >"$work/out" 2>&1
}
# expect_config <name> <initial content or "-" for no file> <want JSON>
expect_config() {
  local cfg="$work/etc/docker/daemon.json" got want
  rm -rf "$work/etc"
  if [ "$2" != - ]; then mkdir -p "$work/etc/docker"; printf '%s' "$2" >"$cfg"; fi
  if run_action "$cfg"; then
    got=$(jq -cS . "$cfg") && want=$(jq -cS . <<<"$3")
    if [ "$got" = "$want" ] && grep -qx 'systemctl restart docker' "$work/stub.log"; then ok "$1"; else
      echo "got  $got" >>"$work/out"; echo "want $want" >>"$work/out"; cat "$work/stub.log" >>"$work/out"
      fail "$1" "$work/out"
    fi
  else
    fail "$1: the script failed" "$work/out"
  fi
}
m='https://mirror.gcr.io'
expect_config "no daemon.json: created with the mirror" - "{\"registry-mirrors\":[\"$m\"]}"
expect_config "an empty daemon.json is treated as {}" "" "{\"registry-mirrors\":[\"$m\"]}"
expect_config "other keys are kept and an existing mirror comes after ours" \
  '{"exec-opts":["native.cgroupdriver=cgroupfs"],"log-level":"warn","registry-mirrors":["https://other.example"]}' \
  "{\"exec-opts\":[\"native.cgroupdriver=cgroupfs\"],\"log-level\":\"warn\",\"registry-mirrors\":[\"$m\",\"https://other.example\"]}"
expect_config "ours already listed (last): moved first, not duplicated" \
  "{\"registry-mirrors\":[\"https://other.example\",\"$m\"]}" "{\"registry-mirrors\":[\"$m\",\"https://other.example\"]}"
cfg="$work/etc/docker/daemon.json"
run_action "$cfg" && cp "$cfg" "$work/once.json" && run_action "$cfg" && cmp -s "$cfg" "$work/once.json" &&
  ok "a second run changes nothing" || fail "a second run changes daemon.json" "$cfg"
grep -q 'docker registry mirrors: \["https://mirror.gcr.io/"' "$work/out" &&
  ok "the step prints the mirrors docker reports" || fail "the step does not print the mirrors" "$work/out"

mkdir -p "$work/etc/docker"
printf '{"registry-mirrors":' >"$cfg"
if run_action "$cfg"; then fail "invalid JSON was accepted"; else
  [ "$(cat "$cfg")" = '{"registry-mirrors":' ] && ! grep -q systemctl "$work/stub.log" &&
    ok "invalid daemon.json fails the step before it writes anything or restarts docker" ||
    fail "invalid daemon.json was changed or docker restarted" "$work/stub.log"
fi
rm -f "$cfg"
if STUB_SYSTEMCTL_FAIL=1 run_action "$cfg"; then fail "a failed docker restart was ignored"; else ok "a failed docker restart fails the step"; fi
if STUB_DOCKER_DOWN=1 run_action "$cfg"; then fail "a docker that never comes back was ignored"; else ok "a docker that does not answer after the restart fails the step"; fi
if STUB_NO_MIRROR=1 run_action "$cfg"; then fail "a docker that reports no mirror was accepted"; else
  grep -q 'not using mirror.gcr.io' "$work/out" && ok "a docker that reports no mirror fails the step" ||
    fail "a docker without the mirror failed for another reason" "$work/out"
fi

# --- the workflows -----------------------------------------------------------

# audit <workflow>: "need <job>" for each job with a step that builds an
# image, creates a kind cluster or otherwise pulls a container image, and
# "bad <file>:<line>: <job>: <why>" for each violation, and "buildkit
# <digest>" for each buildx step's builder image.
audit() {
  awk -v file="$1" '
    # Is this image reference served by Docker Hub (no registry host, or one
    # of its names)? An expression with no host counts: it cannot be told.
    function hub(img,   first) {
      if (img !~ /\//) return 1
      first = img; sub(/\/.*$/, "", first)
      if (first == "docker.io" || first == "index.docker.io" || first == "registry-1.docker.io") return 1
      if (first ~ /[.:]/ || first == "localhost") return 0
      return 1
    }
    function image(img, line) {
      sub(/^[ ]+/, "", img); sub(/[ ]+#.*$/, "", img); gsub(/["\047]/, "", img)
      if (hub(img)) print "bad " file ":" line ": " job ": image " img " would be pulled from Docker Hub; name it mirror.gcr.io/..."
    }
    function needs(t) {
      return (t ~ /(^|[^[:alnum:]_.\/-])docker[[:space:]]+(buildx[[:space:]]+build|build|pull|run|create)([[:space:]]|$)/ ||
        t ~ /docker\/setup-buildx-action@/ || t ~ /goreleaser\/goreleaser-action@/ ||
        t ~ /create[[:space:]]+cluster/ ||
        t ~ /make[[:space:]]+(images|e2e|agent-e2e|release-check|docker-build|kind-load|demo-up)([[:space:]]|$)/ ||
        t ~ /hack\/(e2e|images)\.sh/ || t ~ /hack\/demo\/kind-setup\.sh/ || t ~ /uses:[ ]*docker:\/\//)
    }
    function endstep() {
      if (!sn) return
      if (st ~ /uses:[ ]*actions\/checkout@/ && !checkout_n) checkout_n = n
      if (st ~ /uses:[ ]*\.\/\.github\/actions\/dockerhub-mirror([ ]|\n|$)/) {
        if (!mirror_n) { mirror_n = n; mirror_line = sn }
      } else if (!need_n && needs(st)) {
        need_n = n; need_line = sn
      }
      if (st ~ /docker\/setup-buildx-action@/) {
        if (st !~ /buildkitd-config-inline:/ || st !~ /\[registry\."docker\.io"\]/ || st !~ /mirrors = \["mirror\.gcr\.io"\]/)
          print "bad " file ":" sn ": " job ": docker/setup-buildx-action without buildkitd-config-inline sending docker.io to mirror.gcr.io (its BuildKit ignores the daemon mirror)"
        if (match(st, /driver-opts:[ ]*image=moby\/buildkit:buildx-stable-1@sha256:[0-9a-f]+/)) {
          d = substr(st, RSTART, RLENGTH); sub(/^.*@/, "", d)
          if (length(d) == 71) print "buildkit " d
          else print "bad " file ":" sn ": " job ": the BuildKit image digest " d " is not sha256 and 64 hex digits"
        } else
          print "bad " file ":" sn ": " job ": docker/setup-buildx-action without driver-opts: image=moby/buildkit:buildx-stable-1@sha256:<digest> (a moving tag is not served from the mirror cache)"
      }
      sn = 0
    }
    function endjob() {
      endstep()
      if (!job) return
      if (need_n) print "need " job
      if (need_n && !mirror_n)
        print "bad " file ":" need_line ": " job ": a step builds, pulls or runs a container image or creates a kind cluster, and the job has no ./.github/actions/dockerhub-mirror step"
      else if (need_n && mirror_n > need_n)
        print "bad " file ":" mirror_line ": " job ": the dockerhub-mirror step comes after the first docker step (line " need_line ")"
      if (mirror_n && (!checkout_n || checkout_n > mirror_n))
        print "bad " file ":" mirror_line ": " job ": the dockerhub-mirror step is not after actions/checkout (a local action needs the tree)"
      if (svc && mirror_n)
        print "bad " file ":" mirror_line ": " job ": has services: or container:, which start before any step; the mirror step restarts docker and would kill them (name their images mirror.gcr.io/...)"
      job = ""
    }
    /^[ ]*#/ { next }
    /^jobs:/ { injobs = 1; next }
    /^[^ ]/ { endjob(); injobs = 0 }
    !injobs { next }
    /^  [A-Za-z0-9_-]+:[ ]*(#.*)?$/ {
      endjob(); job = $1; sub(/:$/, "", job)
      n = 0; sn = 0; mirror_n = 0; need_n = 0; checkout_n = 0; svc = 0; blk = 0; insteps = 0
      next
    }
    /^    [^ ]/ {
      endstep(); insteps = ($0 ~ /^    steps:/); blk = 0
      if ($0 ~ /^    (services|container):[ ]*(#.*)?$/) { svc = 1; blk = 1 }
      else if ($0 ~ /^    container:[ ]*[^ #]/) { svc = 1; v = $0; sub(/^    container:[ ]*/, "", v); image(v, NR) }
      next
    }
    blk && /^ +image:/ { v = $0; sub(/^ +image:/, "", v); image(v, NR); next }
    insteps && /^      - / { endstep(); sn = NR; n++; st = $0; next }
    sn { st = st "\n" $0 }
    END { endjob() }
  ' "$1"
}
bad_of() { audit "$1" | sed -n 's/^bad //p'; }
# why: the first violation in $work/out, as "<job>: <reason>" cut to one line.
why() { head -1 "$work/out" | sed 's/^[^ ]* //' | cut -c1-110; }
need_of() { audit "$1" | sed -n 's/^need //p' | sort | tr '\n' ' ' | sed 's/ $//'; }

for f in .github/workflows/*.yml; do
  bad_of "$f" >"$work/out"
  [ ! -s "$work/out" ] && ok "$f: every job that pulls from Docker Hub pulls through the mirror" ||
    fail "$f: a job pulls from Docker Hub without the mirror" "$work/out"
done
# One builder image digest across every workflow, and the audit sees every
# buildx step (3: images and release-check in ci.yml, goreleaser in release.yml).
digests() { local f; for f in "$@"; do audit "$f" | sed -n 's/^buildkit //p'; done | sort | uniq -c | sed 's/^ *//'; }
digests .github/workflows/*.yml >"$work/out"
if [ "$(wc -l <"$work/out" | tr -d ' ')" = 1 ] && grep -qE '^3 sha256:[0-9a-f]{64}$' "$work/out"; then
  ok "the 3 buildx steps pull the BuildKit image by one digest ($(cut -d' ' -f2 "$work/out"))"
else
  fail "the buildx steps do not all pull the BuildKit image by the same digest" "$work/out"
fi
# The audit has to see the jobs it is there for.
want_need() {
  local got
  got=$(need_of "$2")
  if [ "$got" = "$3" ]; then ok "$1: the audit finds the jobs that use Docker ($got)"; else fail "$1: the audit finds '$got', want '$3'"; fi
}
want_need "ci.yml" "$ci" "images kube release-check"
want_need "release.yml" "$release" "goreleaser verify"
for f in docs kb-refresh pr-lint scorecard vuln-latest-release; do
  want_need "$f.yml" ".github/workflows/$f.yml" ""
done
[ "$(grep -c '^      - uses: \./\.github/actions/dockerhub-mirror' "$ci")" = 3 ] &&
  [ "$(grep -c '^      - uses: \./\.github/actions/dockerhub-mirror' "$release")" = 2 ] &&
  ok "ci.yml has 3 mirror steps (images, release-check, kube) and release.yml 2 (goreleaser, verify)" ||
  fail "unexpected number of dockerhub-mirror steps"

# --- the rules fail on a mutated or extended workflow -------------------------

# mutant <name> <workflow> <perl args...>: the audit must report something.
mutant() {
  local name=$1 file=$2
  shift 2
  perl "$@" "$file" >"$work/m.yml"
  if cmp -s "$file" "$work/m.yml"; then fail "mutant '$name' changed nothing (its pattern no longer matches)"; return; fi
  bad_of "$work/m.yml" >"$work/out"
  if [ -s "$work/out" ]; then ok "caught: $name -> $(why)"; else fail "not caught: $name"; fi
}
mirror_re='^      - uses: \./\.github/actions/dockerhub-mirror'
for k in 1 2 3; do
  mutant "ci.yml with mirror step $k deleted" "$ci" -ne "print unless m{$mirror_re} && ++\$n == $k"
done
for k in 1 2; do
  mutant "release.yml with mirror step $k deleted" "$release" -ne "print unless m{$mirror_re} && ++\$n == $k"
done
mutant "ci.yml images job: mirror step after the buildx step" "$ci" -0777 -pe \
  's{(      - uses: \./\.github/actions/dockerhub-mirror[^\n]*\n)(.*?)(      - name: build \$\{\{ matrix\.dockerfile)}{$2$1$3}s'
mutant "ci.yml images job: mirror step before checkout" "$ci" -0777 -pe \
  's{(      - uses: actions/checkout\@[^\n]*\n        with:\n          persist-credentials: false\n)(      - uses: \./\.github/actions/dockerhub-mirror[^\n]*\n)}{$2$1}'
mutant "ci.yml buildx step without buildkitd-config-inline" "$ci" -0777 -pe 's/ {10}buildkitd-config-inline: \|\n( {12,}.*\n)+//'
mutant "release.yml buildx step without buildkitd-config-inline" "$release" -0777 -pe 's/ {10}buildkitd-config-inline: \|\n( {12,}.*\n)+//'
mutant "ci.yml buildx step without driver-opts" "$ci" -ne 'print unless m{^          driver-opts: image=} && ++$n == 1'
mutant "release.yml buildx step without driver-opts" "$release" -ne 'print unless m{^          driver-opts: image=}'
mutant "buildx step on the moving buildx-stable-1 tag" "$ci" -pe 's{(driver-opts: image=moby/buildkit:buildx-stable-1)\@sha256:[0-9a-f]+}{$1}'
mutant "buildx step on a truncated digest" "$ci" -pe 's{(driver-opts: image=moby/buildkit:buildx-stable-1\@sha256:[0-9a-f]{12})[0-9a-f]+}{$1}'
perl -pe '$d ||= s{(driver-opts: image=moby/buildkit:buildx-stable-1\@sha256:)[0-9a-f]{8}}{${1}00000000}' "$ci" >"$work/m.yml"
digests "$work/m.yml" "$release" >"$work/out"
[ "$(wc -l <"$work/out" | tr -d ' ')" -gt 1 ] && ok "caught: a buildx step with another builder image digest than the rest" ||
  fail "not caught: a buildx step with another builder image digest"
mutant "buildkitd config naming another mirror" "$ci" -pe 's/mirrors = \["mirror\.gcr\.io"\]/mirrors = ["registry.example"]/'
mutant "buildkitd config for another registry" "$ci" -pe 's/\[registry\."docker\.io"\]/[registry."ghcr.io"]/'
mutant "pg-conformance service image straight from Docker Hub" "$ci" -pe 's{image: mirror\.gcr\.io/library/\$\{\{ matrix\.image \}\}}{image: \${{ matrix.image }}}'
mutant "pg-conformance with the mirror step (its restart would kill the service)" "$ci" -0777 -pe \
  's{(  pg-conformance:\n.*?persist-credentials: false\n)}{$1      - uses: ./.github/actions/dockerhub-mirror\n}s'

# extended <workflow> <step lines...>: the workflow plus one more job, with
# those steps (a single-quoted YAML step per argument).
extended() {
  local file=$1
  shift
  { cat "$file"; printf '\n  extra:\n    runs-on: ubuntu-latest\n    steps:\n'; printf '%s\n' "$@"; } >"$work/m.yml"
}
co='      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1'
mi='      - uses: ./.github/actions/dockerhub-mirror'
sbx='      - uses: docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069 # v4.4.1'
bk='          driver-opts: image=moby/buildkit:buildx-stable-1@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea'
cfg_inline="$bk
          buildkitd-config-inline: |
            [registry.\"docker.io\"]
              mirrors = [\"mirror.gcr.io\"]"
# new_job <name> <command>: a new job whose run step is <command>.
new_job() {
  local verdict=$1 name=$2 cmd=$3
  extended "$ci" "$co" "      - run: $cmd"
  bad_of "$work/m.yml" >"$work/out"
  if [ "$verdict" = caught ]; then
    if [ -s "$work/out" ]; then ok "caught: a new job that runs $name -> $(why)"; else fail "not caught: a new job that runs $name"; fi
  elif [ ! -s "$work/out" ]; then ok "not flagged: a new job that runs $name"; else fail "wrongly flagged: a new job that runs $name" "$work/out"; fi
}
new_job caught "docker build" 'docker build -t x .'
new_job caught "docker buildx build" 'docker buildx build --platform linux/amd64 .'
new_job caught "docker pull" 'docker pull postgres:17'
new_job caught "docker run" 'docker run --rm alpine true'
new_job caught "docker create" 'docker create alpine'
new_job caught "kind create cluster" 'kind create cluster'
new_job caught "a kind binary variable" '"$kind" create cluster --name x'
new_job caught "make images" 'make images'
new_job caught "make e2e" 'make e2e E2E_MINOR=1.31'
new_job caught "make release-check" 'make release-check'
new_job caught "hack/e2e.sh" 'hack/e2e.sh'
new_job not "docker buildx imagetools (registry API only)" 'docker buildx imagetools inspect ghcr.io/x/y:1'
new_job not "docker login" 'docker login ghcr.io'
new_job not "make test" 'make test'
extended "$ci" "$co" '      - name: only a comment mentions it
        run: |
          # docker build is not run here
          echo hi'
bad_of "$work/m.yml" >"$work/out"
[ ! -s "$work/out" ] && ok "not flagged: a comment that mentions docker build" || fail "wrongly flagged: a comment that mentions docker build" "$work/out"
extended "$ci" "$co" "$mi" '      - run: docker build -t x .'
bad_of "$work/m.yml" >"$work/out"
[ ! -s "$work/out" ] && ok "not flagged: docker build after the mirror step" || fail "wrongly flagged: docker build after the mirror step" "$work/out"
extended "$ci" "$co" "$mi" "$sbx" '        with:
          cache-binary: false'
bad_of "$work/m.yml" >"$work/out"
[ -s "$work/out" ] && ok "caught: a new buildx step without the BuildKit mirror" || fail "not caught: a new buildx step without the BuildKit mirror"
extended "$ci" "$co" "$mi" "$sbx" "        with:
          cache-binary: false
$cfg_inline"
bad_of "$work/m.yml" >"$work/out"
[ ! -s "$work/out" ] && ok "not flagged: a new buildx step with the BuildKit mirror after the mirror step" ||
  fail "wrongly flagged: a new buildx step with the BuildKit mirror" "$work/out"
extended "$ci" "$co" "$sbx" "        with:
          cache-binary: false
$cfg_inline"
bad_of "$work/m.yml" >"$work/out"
[ -s "$work/out" ] && ok "caught: a new buildx step with no daemon mirror step" || fail "not caught: a new buildx step with no daemon mirror step"
extended "$ci" "$co" '      - uses: goreleaser/goreleaser-action@f06c13b6b1a9625abc9e6e439d9c05a8f2190e94 # v7.2.3'
bad_of "$work/m.yml" >"$work/out"
[ -s "$work/out" ] && ok "caught: a new GoReleaser step (it builds images) without the mirror" || fail "not caught: a new GoReleaser step without the mirror"

# services: and container:
svc_job() { # <verdict> <name> <job text after runs-on>
  { cat "$ci"; printf '\n  extra:\n    runs-on: ubuntu-latest\n'; printf '%s\n' "$3"; printf '    steps:\n%s\n      - run: echo hi\n' "$co"; } >"$work/m.yml"
  bad_of "$work/m.yml" >"$work/out"
  if [ "$1" = caught ]; then
    if [ -s "$work/out" ]; then ok "caught: $2 -> $(why)"; else fail "not caught: $2"; fi
  elif [ ! -s "$work/out" ]; then ok "not flagged: $2"; else fail "wrongly flagged: $2" "$work/out"; fi
}
svc_job caught "a service image from Docker Hub" '    services:
      db:
        image: postgres:17'
svc_job caught "a service image from docker.io by name" '    services:
      db:
        image: docker.io/library/postgres:17'
svc_job caught "a service image that is an expression with no registry" '    services:
      db:
        image: ${{ matrix.image }}'
svc_job caught "a container: image from Docker Hub (block form)" '    container:
      image: node:24'
svc_job caught "a container: image from Docker Hub (inline form)" '    container: node:24'
svc_job not "a service image on mirror.gcr.io" '    services:
      db:
        image: mirror.gcr.io/library/postgres:17'
svc_job not "a service image on ghcr.io" '    services:
      db:
        image: ghcr.io/example/db:1'
svc_job not "a container: image on gcr.io" '    container: gcr.io/example/ci:1'
{ cat "$ci"; printf '\n  extra:\n    runs-on: ubuntu-latest\n    services:\n      db:\n        image: mirror.gcr.io/library/postgres:17\n    steps:\n%s\n%s\n' "$co" "$mi"; } >"$work/m.yml"
bad_of "$work/m.yml" >"$work/out"
[ -s "$work/out" ] && ok "caught: the mirror step in a job with services (the restart would kill them)" ||
  fail "not caught: the mirror step in a job with services"

# --- documented --------------------------------------------------------------

grep -q 'mirror\.gcr\.io' CONTRIBUTING.md && grep -q 'dockerhub-mirror' CONTRIBUTING.md &&
  ok "CONTRIBUTING.md documents the mirror" || fail "CONTRIBUTING.md does not mention mirror.gcr.io and the dockerhub-mirror action"

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "dockerhub-mirror_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
