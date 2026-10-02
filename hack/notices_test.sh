#!/usr/bin/env bash
# Tests for hack/notices.sh (make notices-check runs them first). Each case
# builds a fixture tree (go.mod, web/package-lock.json, web/node_modules), a
# stub go-licenses that prints fixed report rows, breaks one thing, and
# asserts the exit status and message. Offline: no go-licenses, npm or
# module proxy.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# fixture <dir>: a tree whose dependencies all pass the policy. The stub
# reports the rows in <dir>/go-rows (name, version, license, file).
fixture() {
  local d=$1
  mkdir -p "$d/licenses" "$d/web/node_modules/left-pad" "$d/web/node_modules/vite" "$d/web/node_modules/typescript"
  printf 'module example.com/x\n\ngo 1.26.8\n' >"$d/go.mod"
  printf 'Go license text\n' >"$d/licenses/go"
  printf 'MIT License\r\nCopyright a   \r\n' >"$d/licenses/a"
  printf 'Apache License 2.0\n' >"$d/licenses/apache"
  printf 'example.com/a\tv1.0.0\tMIT\t%s\n' "$d/licenses/a" >"$d/go-rows"
  printf 'example.com/b\tv2.0.0\tApache-2.0\t%s\n' "$d/licenses/apache" >>"$d/go-rows"
  printf 'example.com/c\tv3.0.0\tApache-2.0\t%s\n' "$d/licenses/apache" >>"$d/go-rows"
  cat >"$d/web/package-lock.json" <<'EOF'
{
  "packages": {
    "": {"name": "dash"},
    "node_modules/left-pad": {"version": "1.3.0", "license": "MIT"},
    "node_modules/vite": {"version": "6.0.0", "license": "MIT", "dev": true},
    "node_modules/typescript": {"version": "5.8.3", "license": "Apache-2.0", "dev": true}
  }
}
EOF
  printf 'left-pad license\n' >"$d/web/node_modules/left-pad/LICENSE"
  printf 'vite license\n' >"$d/web/node_modules/vite/LICENSE.md"
  printf 'typescript license\n' >"$d/web/node_modules/typescript/LICENSE.txt"
  cat >"$d/go-licenses" <<EOF
#!/usr/bin/env bash
cat "$d/go-rows"
EOF
  chmod +x "$d/go-licenses"
}

: >"$work/results"
# expect <name> <want-exit> <want-substring> <fixture-dir> [--check]
expect() {
  local name=$1 want=$2 needle=$3 dir=$4 got=0
  shift 4
  UPGRADESCOPE_NOTICES_ROOT="$dir" UPGRADESCOPE_GO_LICENSES="$dir/go-licenses" \
    UPGRADESCOPE_GO_LICENSE="$dir/licenses/go" \
    hack/notices.sh "$@" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}
# has <name> <file> <substring>: the generated file contains it.
has() {
  if grep -qF -- "$3" "$2"; then
    echo "ok   $1" | tee -a "$work/results"
  else
    echo "FAIL $1: $2 lacks '$3'" >&2
    echo "FAIL $1" >>"$work/results"
  fi
}
case_dir() { local d="$work/$1"; fixture "$d"; echo "$d"; }

d=$(case_dir pass)
expect "allowed licenses generate the file" 0 "notices: wrote THIRD_PARTY_NOTICES" "$d"
n="$d/THIRD_PARTY_NOTICES"
has "Go module listed with its license" "$n" "  example.com/a v1.0.0 (MIT)"
has "Go standard library listed at go.mod's version" "$n" "  Go standard library go1.26.8 (BSD-3-Clause)"
has "production npm package listed" "$n" "  left-pad 1.3.0 (MIT)"
has "bundled dev package (vite) listed" "$n" "  vite 6.0.0 (MIT)"
has "license text reproduced" "$n" "MIT License"
if grep -q "$(printf '\r')" "$n" || grep -q 'Copyright a ' "$n"; then
  echo "FAIL CRLF and trailing blanks normalized" >&2; echo "FAIL CRLF and trailing blanks normalized" >>"$work/results"
else
  echo "ok   CRLF and trailing blanks normalized" | tee -a "$work/results"
fi
if grep -q typescript "$n"; then
  echo "FAIL dev-only npm package excluded" >&2; echo "FAIL dev-only npm package excluded" >>"$work/results"
else
  echo "ok   dev-only npm package excluded" | tee -a "$work/results"
fi
if [ "$(grep -c '^Apache License 2.0$' "$n")" = 1 ]; then
  echo "ok   one shared license text printed once" | tee -a "$work/results"
else
  echo "FAIL one shared license text printed once" >&2; echo "FAIL one shared license text printed once" >>"$work/results"
fi
expect "fresh file passes --check" 0 "THIRD_PARTY_NOTICES is up to date" "$d" --check

d=$(case_dir stale)
expect "generate" 0 "wrote" "$d"
printf 'example.com/new\tv0.1.0\tMIT\t%s\n' "$d/licenses/a" >>"$d/go-rows"
expect "a new dependency makes --check fail" 1 "THIRD_PARTY_NOTICES is stale" "$d" --check

d=$(case_dir gpl)
printf 'example.com/gpl\tv1.0.0\tGPL-3.0\t%s\n' "$d/licenses/a" >>"$d/go-rows"
expect "GPL Go module fails" 1 "example.com/gpl v1.0.0 is licensed 'GPL-3.0', which is not allowed" "$d"

d=$(case_dir agpl-check)
printf 'example.com/agpl\tv1.0.0\tAGPL-3.0\t%s\n' "$d/licenses/a" >>"$d/go-rows"
expect "--check enforces the policy too" 1 "example.com/agpl v1.0.0 is licensed 'AGPL-3.0'" "$d" --check

d=$(case_dir unknown)
printf 'example.com/mystery\tv1.0.0\tUnknown\t%s\n' "$d/licenses/a" >>"$d/go-rows"
expect "unidentified Go license fails" 1 "example.com/mystery v1.0.0 is licensed 'Unknown'" "$d"

d=$(case_dir no-go-file)
printf 'example.com/nofile\tv1.0.0\tMIT\tUnknown\n' >>"$d/go-rows"
expect "Go module without a license file fails" 1 "example.com/nofile v1.0.0: no license file found" "$d"

d=$(case_dir sspl-npm)
sed -i.bak 's/"license": "MIT"}/"license": "SSPL-1.0"}/' "$d/web/package-lock.json" && rm -f "$d/web/package-lock.json.bak"
expect "SSPL npm package fails" 1 "left-pad 1.3.0 is licensed 'SSPL-1.0'" "$d"

d=$(case_dir npm-no-license-field)
sed -i.bak 's/"version": "1.3.0", "license": "MIT"/"version": "1.3.0"/' "$d/web/package-lock.json" && rm -f "$d/web/package-lock.json.bak"
expect "npm package without a license field fails" 1 "left-pad 1.3.0 is licensed 'Unknown'" "$d"

d=$(case_dir npm-no-file)
rm "$d/web/node_modules/left-pad/LICENSE"
expect "npm package without a license file fails" 1 "left-pad 1.3.0: no license file found" "$d"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "notices_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
