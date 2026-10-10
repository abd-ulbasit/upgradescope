#!/usr/bin/env bash
# No job on the release path restores a GitHub Actions cache (IR-22, #244).
# Any job can write the cache with the runner's own token, whatever its
# permissions: block, and a run on a tag restores entries written on main:
# kb-refresh's job that builds freshly bumped, unreviewed modules runs on
# main. So every job that builds, signs or publishes a release, and every
# job of ci.yml that release.yml calls to gate it, restores nothing:
#   - release.yml: setup-go `cache: false`; no setup-node cache; no
#     actions/cache; setup-buildx `cache-binary: false`;
#   - ci.yml: each such step keyed on the workflow's RESTORE_CACHES, which
#     must be false for a release.yml call (inputs.release) and on any tag
#     ref: setup-go `cache: ${{ env.RESTORE_CACHES == 'true' }}`, setup-node
#     `cache: ${{ env.RESTORE_CACHES == 'true' && 'npm' || '' }}` (or none),
#     actions/cache only `if: env.RESTORE_CACHES == 'true'`;
#   - both: setup-node `package-manager-cache: false` (its automatic cache
#     would otherwise turn on with a packageManager field), no `type=gha`
#     BuildKit cache, and every action used is on a list of actions checked
#     to restore no Actions cache of their own, so a new one fails here
#     until someone checks it;
#   - release.yml calls no reusable workflow but ci.yml, with release: true.
# Each rule is also run against a mutated workflow that must fail it.
# Offline; needs node (for the RESTORE_CACHES expression).
set -euo pipefail
cd "$(dirname "$0")/.."

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

# Actions checked to restore no GitHub Actions cache of their own (tool
# caches are the runner's local disk, not the shared Actions cache).
# setup-go, setup-node, setup-buildx and actions/cache have rules below.
known='actions/checkout actions/upload-artifact actions/download-artifact
actions/attest-build-provenance dorny/paths-filter azure/setup-helm
docker/login-action sigstore/cosign-installer anchore/sbom-action/download-syft
goreleaser/goreleaser-action github/codeql-action/upload-sarif
rajatjindal/krew-release-bot ./action ./ ./.github/actions/dockerhub-mirror'

# steps <workflow>: one tab-separated line per step: line, uses, with.cache,
# with.package-manager-cache, with.cache-binary, if (values without their
# trailing comment; "-" when absent).
steps() {
  awk '
    function val(l) { sub(/^[^:]*:[ ]*/, "", l); sub(/[ ]+#.*$/, "", l); return l }
    function flush() { if (n) printf "%s\t%s\t%s\t%s\t%s\t%s\n", n, u, c, p, b, f; n = 0 }
    /^      - / { flush(); n = NR; u = "-"; c = "-"; p = "-"; b = "-"; f = "-" }
    /^  [^ ]/ || /^[^ ]/ { flush() }
    n && /^      (- |  )uses: / { u = val($0); sub(/^- /, "", u) }
    n && /^          cache: / { c = val($0) }
    n && /^          package-manager-cache: / { p = val($0) }
    n && /^          cache-binary: / { b = val($0) }
    n && /^      (- |  )if: / { f = val($0) }
    END { flush() }
  ' "$1"
}

# audit <workflow> <release|gated>: each step that could restore a cache,
# one line per violation (none: the workflow passes).
audit() {
  local file=$1 mode=$2 line uses cache pmc bin cond name
  while IFS=$'\t' read -r line uses cache pmc bin cond; do
    [ "$uses" != - ] || continue
    name=${uses%%@*}
    case $name in
      actions/setup-go)
        if [ "$mode" = release ]; then
          [ "$cache" = false ] || echo "$file:$line: setup-go without cache: false"
        else
          [ "$cache" = "\${{ env.RESTORE_CACHES == 'true' }}" ] ||
            echo "$file:$line: setup-go cache is '$cache', not \${{ env.RESTORE_CACHES == 'true' }}"
        fi
        ;;
      actions/setup-node)
        [ "$pmc" = false ] || echo "$file:$line: setup-node without package-manager-cache: false"
        if [ "$mode" = release ]; then
          [ "$cache" = - ] || echo "$file:$line: setup-node with cache: $cache"
        else
          [ "$cache" = - ] || [ "$cache" = "\${{ env.RESTORE_CACHES == 'true' && 'npm' || '' }}" ] ||
            echo "$file:$line: setup-node cache is '$cache', not keyed on RESTORE_CACHES"
        fi
        ;;
      docker/setup-buildx-action)
        [ "$bin" = false ] || echo "$file:$line: setup-buildx-action without cache-binary: false"
        ;;
      actions/cache | actions/cache/restore)
        if [ "$mode" = release ]; then
          echo "$file:$line: $name on the release path"
        else
          [ "$cond" = "env.RESTORE_CACHES == 'true'" ] ||
            echo "$file:$line: $name runs without if: env.RESTORE_CACHES == 'true' (if: $cond)"
        fi
        ;;
      *)
        grep -qxF -- "$name" <<<"$(tr ' ' '\n' <<<"$known")" ||
          echo "$file:$line: $name is not known to restore no Actions cache; check it and add it to this test's list"
        ;;
    esac
  done < <(steps "$file")
  if grep -n 'type=gha' "$file"; then echo "$file: a type=gha BuildKit cache"; fi
}

# restore <env-expression> <context-json>: the expression's value.
restore() {
  node -e '
    const [src, ctxJSON] = process.argv.slice(1);
    const ctx = JSON.parse(ctxJSON);
    const e = src.trim().replace(/^\$\{\{([\s\S]*)\}\}$/, "$1");
    process.stdout.write(String(new Function("github", "inputs", "return (" + e + ");")(ctx.github, ctx.inputs)));
  ' "$1" "$2"
}
expr_of() { sed -n 's/^  RESTORE_CACHES: //p' "$1"; }
# env_cases <workflow> <label>: RESTORE_CACHES false on the release path,
# true elsewhere. Prints one line per wrong value.
env_cases() {
  local e got
  e=$(expr_of "$1")
  [ -n "$e" ] || { echo "$1 sets no workflow-level RESTORE_CACHES"; return; }
  while read -r want ctx; do
    got=$(restore "$e" "$ctx") || got="error"
    [ "$got" = "$want" ] || echo "RESTORE_CACHES is $got for $ctx, want $want"
  done <<'EOF'
false {"github":{"event_name":"push","ref_type":"tag"},"inputs":{"release":true}}
false {"github":{"event_name":"workflow_dispatch","ref_type":"tag"},"inputs":{"release":true}}
false {"github":{"event_name":"workflow_dispatch","ref_type":"tag"},"inputs":{"full-matrix":true}}
false {"github":{"event_name":"workflow_dispatch","ref_type":"branch"},"inputs":{"release":true}}
true {"github":{"event_name":"pull_request","ref_type":"branch"},"inputs":{}}
true {"github":{"event_name":"push","ref_type":"branch"},"inputs":{}}
true {"github":{"event_name":"schedule","ref_type":"branch"},"inputs":{}}
true {"github":{"event_name":"workflow_dispatch","ref_type":"branch"},"inputs":{"full-matrix":false}}
EOF
}

# --- the real workflows ------------------------------------------------------

audit "$release" release >"$work/out"
[ ! -s "$work/out" ] && ok "release.yml: no step restores an Actions cache" || fail "release.yml restores a cache" "$work/out"
audit "$ci" gated >"$work/out"
[ ! -s "$work/out" ] && ok "ci.yml: every step that could restore a cache is keyed on RESTORE_CACHES" ||
  fail "ci.yml has a cache not keyed on RESTORE_CACHES" "$work/out"
env_cases "$ci" >"$work/out"
[ ! -s "$work/out" ] && ok "ci.yml: RESTORE_CACHES is false for a release.yml call and on any tag ref, true otherwise" ||
  fail "ci.yml's RESTORE_CACHES is wrong for some run" "$work/out"
[ "$(grep -c 'RESTORE_CACHES:' "$ci")" = 1 ] && ok "ci.yml sets RESTORE_CACHES once, at the top (no job overrides it)" ||
  fail "ci.yml sets RESTORE_CACHES more than once"
n=$(steps "$ci" | awk -F'\t' '$2 ~ /^actions\/setup-go@/' | wc -l | tr -d ' ')
[ "$n" -ge 10 ] && ok "ci.yml: the audit sees its setup-go steps ($n)" || fail "ci.yml: the audit sees only $n setup-go steps"
calls=$(grep -E '^    uses: ' "$release" | sed 's/^    uses: //; s/ *#.*//')
if [ "$calls" = ./.github/workflows/ci.yml ] && awk '/^  ci:$/ { j = 1; next } j && /^  [^ ]/ { j = 0 } j && /^      release: true/ { f = 1 } END { exit !f }' "$release"; then
  ok "release.yml calls only ci.yml, with release: true"
else
  echo "$calls" >"$work/out"
  fail "release.yml calls a workflow this test does not audit, or ci.yml without release: true" "$work/out"
fi

# --- each rule fails on a mutated workflow ---------------------------------

# mutant <name> <workflow> <mode> <perl -pe script>: the rule must report
# it. $first is the script's guard for "only the first match".
mutant() {
  local name=$1 file=$2 mode=$3
  perl -pe "$4" "$file" >"$work/m.yml"
  if cmp -s "$file" "$work/m.yml"; then fail "mutant '$name' changed nothing (its pattern no longer matches)"; return; fi
  audit "$work/m.yml" "$mode" >"$work/out"
  if [ -s "$work/out" ]; then ok "caught: $name"; else fail "not caught: $name"; fi
}
go_on='          cache: ${{ env.RESTORE_CACHES == '"'true'"' }}'
node_on='          cache: ${{ env.RESTORE_CACHES == '"'true'"' && '"'npm'"' || '"''"' }}'
mutant "release.yml setup-go with its cache on" "$release" release 's/^          cache: false$/          cache: true/'
mutant "release.yml setup-go with no cache: line" "$release" release '$_ = "" if /^          cache: false$/'
mutant "release.yml with an actions/cache step" "$release" release \
  's|^      - uses: sigstore/cosign-installer@.*|      - uses: actions/cache\@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0|'
mutant "release.yml setup-buildx without cache-binary: false" "$release" release '$_ = "" if /^          cache-binary: false/'
mutant "release.yml with an unchecked action" "$release" release \
  's|^      - uses: sigstore/cosign-installer@|      - uses: someone/setup-thing@|'
mutant "a type=gha BuildKit cache" "$release" release 's|^          args: release --clean$|          args: release --clean --cache-from type=gha|'
mutant "ci.yml setup-go with its cache always on" "$ci" gated \
  'if (!$done && index($_, q('"$go_on"')) == 0) { $_ = "          cache: true\n"; $done = 1 }'
mutant "ci.yml setup-go with no cache: line" "$ci" gated \
  'if (!$done && index($_, q('"$go_on"')) == 0) { $_ = ""; $done = 1 }'
mutant "ci.yml setup-node cache: npm" "$ci" gated \
  'if (!$done && index($_, q('"$node_on"')) == 0) { $_ = "          cache: npm\n"; $done = 1 }'
mutant "ci.yml setup-node without package-manager-cache: false" "$ci" gated \
  'if (!$done && /^          package-manager-cache: false/) { $_ = ""; $done = 1 }'
mutant "ci.yml actions/cache without its if:" "$ci" gated '$_ = "" if /^        if: env.RESTORE_CACHES == .true./'
mutant "ci.yml with actions/cache/restore" "$ci" gated \
  's|^      - uses: actions/cache@|      - uses: actions/cache/restore@|; $_ = "" if /^        if: env.RESTORE_CACHES == .true./'
sed "s/^  RESTORE_CACHES: .*/  RESTORE_CACHES: \${{ github.ref_type != 'tag' }}/" "$ci" >"$work/m.yml"
env_cases "$work/m.yml" >"$work/out"
[ -s "$work/out" ] && ok "caught: RESTORE_CACHES true for a release.yml call that is not on a tag" ||
  fail "not caught: RESTORE_CACHES ignoring inputs.release"
sed "s/^  RESTORE_CACHES: .*/  RESTORE_CACHES: \${{ !inputs.release }}/" "$ci" >"$work/m.yml"
env_cases "$work/m.yml" >"$work/out"
[ -s "$work/out" ] && ok "caught: RESTORE_CACHES true on a tag ref without inputs.release" ||
  fail "not caught: RESTORE_CACHES ignoring the tag ref"

pass=$(grep -c '^ok' "$work/results" || true)
nfail=$(grep -c '^FAIL' "$work/results" || true)
echo "release-caches_test: $pass passed, $nfail failed"
[ "$nfail" -eq 0 ] && [ "$pass" -gt 0 ]
