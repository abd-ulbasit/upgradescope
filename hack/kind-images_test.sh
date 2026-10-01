#!/usr/bin/env bash
# Tests for hack/kind-images.sh against a fixture table and the real one.
# Offline; needs only bash and jq.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cat >"$work/table.txt" <<'EOF'
# comment
1.29 kindest/node:v1.29.14@sha256:8703bd94ee24e51b778d5556ae310c6c0fa67d761fae6379c8e0bb480e6fea29 weekly v0.27.0
1.31 kindest/node:v1.31.14@sha256:6f86cf509dbb42767b6e79debc3f2c32e4ee01386f0489b3b2be24b0a55aac2b pr v0.31.0

1.37 kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5 pr v0.33.0
EOF

: >"$work/results"
# expect <name> <want-exit> <want-output-substring> <args...>
expect() {
  local name=$1 want=$2 needle=$3 got=0
  shift 3
  KIND_IMAGES_TABLE="${TABLE:-$work/table.txt}" hack/kind-images.sh "$@" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

expect "pr matrix is the pr rows" 0 '["1.31","1.37"]' matrix pr
expect "full matrix is every row, in order" 0 '["1.29","1.31","1.37"]' matrix all
expect "image of a minor is its digest-pinned image" 0 \
  'kindest/node:v1.31.14@sha256:6f86cf509dbb42767b6e79debc3f2c32e4ee01386f0489b3b2be24b0a55aac2b' image 1.31
expect "unknown minor fails" 1 "no kind node image for 1.30" image 1.30
expect "unknown matrix fails" 2 "usage" matrix nightly
expect "next minor" 0 "1.32" next 1.31

printf '1.30 kindest/node:v1.30.13 pr v0.29.0\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "image without a digest is rejected" 2 "not pinned by digest" matrix all
printf '1.30 kindest/node:v1.30.13@sha256:397209b3d947d154f6641f2d0ce8d473732bd91c87d9575ade99049aa33cd648 sometimes v0.29.0\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "bad schedule column is rejected" 2 "pr or weekly" matrix all
printf '1.30 kindest/node:v1.31.14@sha256:6f86cf509dbb42767b6e79debc3f2c32e4ee01386f0489b3b2be24b0a55aac2b pr v0.31.0\n' >"$work/bad.txt"
TABLE="$work/bad.txt" expect "image of another minor is rejected" 2 "is not a 1.30 image" matrix all

# The real table: parses, and the PR matrix covers the issue #81 floor and
# ceiling (the oldest minor with the flowcontrol v1beta3 window, the newest).
TABLE=hack/kind-node-images.txt expect "real table: pr matrix" 0 '["1.31","1.37"]' matrix pr
TABLE=hack/kind-node-images.txt expect "real table: weekly covers 1.29..1.37" 0 \
  '["1.29","1.30","1.31","1.32","1.33","1.34","1.35","1.36","1.37"]' matrix all

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "kind-images_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
