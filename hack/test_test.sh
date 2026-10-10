#!/usr/bin/env bash
# Tests for hack/test.sh (make hack-test), offline, with a stub `go` for the
# runs: its units are every package of every module; --shard i/n gives each
# to exactly one shard (a package the duration table has never heard of
# too); --readers selects the packages whose tests read the repository
# outside Go code, and the list of those cannot go stale (a package that
# names a docs page, or walks the whole tree, must be on it); and CI's test
# matrix names every shard, so none can go missing. Running the tests is CI's test job.
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pass=0 fail=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1" >&2; fail=$((fail + 1)); }
check() { # <name> <want> <got>
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: want [$(echo $2)] got [$(echo $3)]"; fi
}

all=$(hack/test.sh --list)

# ---- the units ------------------------------------------------------------
# Derived another way: the directories of the tracked Go files (not under
# testdata, nor a _ or . directory, which ./... skips), each in the module
# of its nearest go.mod.
modules=$(git ls-files -- 'go.mod' '*/go.mod' | sed 's#/\{0,1\}go\.mod$##; s#^$#.#')
# unit_of <dir>: "<module dir> <package dir relative to it>" for a directory.
unit_of() {
  local d=$1 best=. m rel
  for m in $modules; do
    [ "$m" = . ] && continue
    if [ "$d" = "$m" ] || [[ "$d" == "$m"/* ]]; then
      if [ "$best" = . ] || [ "${#m}" -gt "${#best}" ]; then best=$m; fi
    fi
  done
  if [ "$best" = . ]; then
    rel=$d
  else
    rel=${d#"$best"}
    rel=${rel#/}
    [ -n "$rel" ] || rel=.
  fi
  echo "$best $rel"
}
derived=$(git ls-files -- '*.go' | awk -F/ '{
    for (i = 1; i < NF; i++) if ($i == "testdata" || $i ~ /^[_.]/) next
    d = $1; for (i = 2; i < NF; i++) d = d "/" $i
    if (NF == 1) d = "."
    print d
  }' | sort -u | while read -r d; do unit_of "$d"; done | sort -u)
check "--list is every package of every module (the tracked Go files' directories)" "$derived" "$(sort <<<"$all")"
if ! grep -q '^tools/promrule-test \.$' <<<"$all" || ! grep -q '^\. internal/server$' <<<"$all"; then
  bad "--list lacks the tools/ modules or the main module's packages"
else
  ok "--list names the main module's packages and each tools/ module's"
fi

# ---- the shards -----------------------------------------------------------
for scope in all readers; do
  flags=()
  [ "$scope" = readers ] && flags=(--readers)
  want=$(hack/test.sh --list ${flags[@]+"${flags[@]}"} | sort)
  for n in 1 2 3 4; do
    got=$(for i in $(seq 1 "$n"); do hack/test.sh --list ${flags[@]+"${flags[@]}"} --shard "$i/$n" 2>/dev/null; done | sort)
    if [ -n "$(uniq -d <<<"$got")" ]; then
      bad "$scope, $n shards: a package is in two shards: $(uniq -d <<<"$got" | head -2 | tr '\n' ' ')"
    elif [ "$got" != "$want" ]; then
      bad "$scope, $n shards: the union is not the whole set"
    else
      ok "$scope, $n shards: every package is in exactly one ($(wc -l <<<"$got" | tr -d ' '))"
    fi
  done
done
for i in 1 2; do
  [ -n "$(hack/test.sh --list --shard "$i/2" 2>/dev/null)" ] && ok "shard $i/2 is not empty" || bad "shard $i/2 is empty"
done

# The tables: "<module> <package> <seconds>" lines, no package twice; a line
# for a package that is gone only costs balance, so it is noted, not failed.
if lines=$(grep -vE '^(#.*|[[:space:]]*|[^ ]+ [^ ]+ [0-9]+)$' hack/test-durations.txt); then
  bad "hack/test-durations.txt has a line that is not '<module> <package> <seconds>': $lines"
else
  ok "hack/test-durations.txt holds only '<module> <package> <seconds>' lines"
fi
if dup=$(grep -vE '^(#|[[:space:]]*$)' hack/test-durations.txt | awk '{print $1, $2}' | sort | uniq -d | grep .); then
  bad "hack/test-durations.txt lists a package twice: $dup"
else
  ok "hack/test-durations.txt lists no package twice"
fi
stale=$(grep -vE '^(#|[[:space:]]*$)' hack/test-durations.txt | awk '{print $1, $2}' | sort | comm -23 - <(sort <<<"$all"))
[ -z "$stale" ] || echo "note: hack/test-durations.txt has lines for packages that are gone (balance only): $(echo $stale)"
unknown=$(comm -13 <(grep -vE '^(#|[[:space:]]*$)' hack/test-durations.txt | awk '{print $1, $2}' | sort) <(sort <<<"$all"))
[ -z "$unknown" ] || echo "note: packages with no line in hack/test-durations.txt (10 s assumed): $(echo $unknown)"

# ---- the readers -----------------------------------------------------
readers=$(sed -e 's/#.*//' -e '/^[[:space:]]*$/d' hack/test-readers.txt | sort)
if lines=$(grep -vE '^[^ ]+ [^ ]+$' <<<"$readers"); then
  bad "hack/test-readers.txt has a line that is not '<module> <package>': $lines"
else
  ok "hack/test-readers.txt holds only '<module> <package>' lines"
fi
# A reader that is not a package of the tree would silently drop its tests
# from the docs run (a rename): fail on it.
gone=$(comm -23 <(echo "$readers") <(sort <<<"$all"))
if [ -n "$gone" ]; then bad "hack/test-readers.txt names packages that do not exist: $(echo $gone)"; else ok "every reader is a package of the tree"; fi
# Every package that names a docs page or a Markdown file in a string is on
# the list. The reading is by path, from the package's own directory, so a
# string is how a test finds a page: docs/..., ../../README.md, a name like
# SECURITY.md, mkdocs.yml, hack/docs. Comment lines are not read.
re='"(\.\./)*(docs|hack/docs)(/|")|"(\.\./)+[A-Za-z_./-]*\.md"|"[A-Z][A-Z_]*\.md"|"(\.\./)*mkdocs\.yml"|`(\.\./)*docs/'
detected=$(git ls-files -z -- '*.go' | xargs -0 grep -nE "$re" 2>/dev/null |
  grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | cut -d: -f1 | sort -u | while read -r f; do unit_of "$(dirname "$f")"; done | sort -u)
missing=$(comm -23 <(echo "$detected") <(echo "$readers"))
if [ -n "$missing" ]; then
  bad "packages that name a docs page or Markdown file are not in hack/test-readers.txt (a docs-only change would skip their tests): $(echo $missing)"
else
  ok "every package that names a docs page is a docs reader ($(wc -l <<<"$detected" | tr -d ' ') found)"
fi

# ...and so is every package whose tests walk or list the checkout from
# outside their own directory. Such a test reads files nobody named: it
# decodes every YAML file under the root, or every tracked file, so a change
# to ANY file (a workflow, an issue template, a script) can fail it, and
# deleting files to see what breaks cannot find it (it skips what is missing).
# This is why a pull request that changes no Go code still runs this list
# (CI's test-scope "readers"), never nothing. The heuristic: a WalkDir or
# Walk whose root is a "../" path or a variable called root, repoRoot, top
# and the like; `git ls-files`; `git rev-parse --show-toplevel`.
walk_re='Walk(Dir)?\((filepath\.Join\()?("\.\./|[A-Za-z]*([Rr]oot|[Tt]op)\b)|DirFS\(("\.\./|[A-Za-z]*([Rr]oot|[Tt]op)\b)|"ls-files"|--show-toplevel'
# The detector itself: lines it must flag, and lines it must leave alone.
detector_bad=0
for line in 'filepath.WalkDir("../..", func(' 'err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(' \
  'filepath.WalkDir(root, func(' 'filepath.Walk(top, fn)' 'exec.Command("git", "ls-files", "-z")' 'exec.Command(git, "-C", root, "ls-files", "-z")' \
  'exec.Command("git", "rev-parse", "--show-toplevel")' 'fs.WalkDir(os.DirFS("../.."), ".", fn)'; do
  grep -qE "$walk_re" <<<"$line" || { bad "the tree-walk detector misses: $line"; detector_bad=1; }
done
for line in 'filepath.WalkDir("testdata/adversarial", fn)' 'filepath.WalkDir(dir, fn)' 'filepath.WalkDir(".", fn)' 'filepath.Glob("testdata/*")' 'os.ReadFile("../engine/testdata/a.json")'; do
  grep -qE "$walk_re" <<<"$line" && { bad "the tree-walk detector flags a read inside the package: $line"; detector_bad=1; }
done
[ "$detector_bad" = 0 ] && ok "the tree-walk detector flags walks from outside a package and leaves its own testdata alone"
walkers=$(git ls-files -z -- '*.go' | xargs -0 grep -nE "$walk_re" 2>/dev/null |
  grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' | cut -d: -f1 | grep '_test\.go$' | sort -u | while read -r f; do unit_of "$(dirname "$f")"; done | sort -u)
[ -n "$walkers" ] || bad "the tree-walk detector found no package: it is broken (internal/server walks the repository)"
missing=$(comm -23 <(echo "$walkers") <(echo "$readers"))
if [ -n "$missing" ]; then
  bad "packages whose tests walk the whole tree are not in hack/test-readers.txt (a change to a file no Go job reads would skip them): $(echo $missing)"
else
  ok "every package that walks the tree is a reader ($(wc -l <<<"$walkers" | tr -d ' ') found)"
fi
for pkg in ". internal/server" ". internal/crd/apigroup"; do
  grep -qxF "$pkg" <<<"$walkers" || bad "the tree-walk detector no longer finds $pkg, which reads every file of the checkout"
done

# ---- refusals -------------------------------------------------------------
for spec in 0/2 3/2 2 a/b -1/2 1/0; do
  if hack/test.sh --list --shard "$spec" >/dev/null 2>&1; then bad "--shard $spec was accepted"; else ok "--shard $spec is refused"; fi
done
if hack/test.sh --list --shard >/dev/null 2>&1; then bad "--shard with no value was accepted"; else ok "--shard with no value is refused"; fi
if hack/test.sh --nope >/dev/null 2>&1; then bad "an unknown flag was accepted"; else ok "an unknown flag is refused"; fi

# ---- running, with a stub go ----------------------------------------------
real_go=$(command -v go)
mkdir -p "$work/bin"
cat >"$work/bin/go" <<'STUB'
#!/usr/bin/env bash
# list goes to the real go; everything else is recorded: "<cwd> <args>".
if [ "$1" = list ]; then exec "$REAL_GO" "$@"; fi
echo "$(pwd -P) $*" >>"$STUB_LOG"
exit "${STUB_RC:-0}"
STUB
chmod +x "$work/bin/go"
root=$(pwd -P)
# units_of_log <log> <verb>: the "<module> <package>" each `go <verb>` covered.
units_of_log() {
  awk -v root="$root" -v verb="$2" '
    $2 == verb {
      mod = $1
      if (mod == root) mod = "."; else if (index(mod, root "/") == 1) mod = substr(mod, length(root) + 2)
      for (i = 3; i <= NF; i++) if ($i ~ /^\.\//) { p = substr($i, 3); if (p == "...") p = "ALL"; print mod, p }
    }' "$1" | while read -r m p; do
    if [ "$p" = ALL ]; then awk -v m="$m" '$1 == m' <<<"$all"; else echo "$m $p"; fi
  done | sort
}
run_stub() { # <log> <args...>
  local log=$1
  shift
  : >"$log"
  REAL_GO=$real_go STUB_LOG=$log PATH="$work/bin:$PATH" hack/test.sh "$@" >"$work/out" 2>&1 || { bad "hack/test.sh $* failed: $(tail -3 "$work/out")"; return 1; }
}

# No flags: the old commands, per module (vet ./... then test -race -count=1
# ./...), the main module first.
if run_stub "$work/log-all"; then
  modcount=$(cut -d' ' -f1 <<<"$all" | sort -u | wc -l | tr -d ' ')
  [ "$(grep -c ' vet \./\.\.\.$' "$work/log-all")" = "$modcount" ] && [ "$(grep -c ' test -race -count=1 \./\.\.\.$' "$work/log-all")" = "$modcount" ] &&
    ok "no flags: go vet ./... and go test -race -count=1 ./... in each of the $modcount modules" ||
    bad "no flags: not the old commands: $(sed "s#$root/##" "$work/log-all" | tr '\n' ';')"
  [ "$(units_of_log "$work/log-all" test)" = "$(sort <<<"$all")" ] && ok "no flags: every package is tested" || bad "no flags: not every package is tested"
fi
# Shards: every package vetted and tested once across them, with the flags.
for n in 2 3; do
  : >"$work/log-n"
  for i in $(seq 1 "$n"); do run_stub "$work/log-$i" --shard "$i/$n" && cat "$work/log-$i" >>"$work/log-n"; done
  [ "$(units_of_log "$work/log-n" test)" = "$(sort <<<"$all")" ] && ok "$n shards run go test on every package exactly once" || bad "$n shards: go test covered $(units_of_log "$work/log-n" test | wc -l | tr -d ' ') of $(wc -l <<<"$all" | tr -d ' ') package runs, or one twice"
  [ "$(units_of_log "$work/log-n" vet)" = "$(sort <<<"$all")" ] && ok "$n shards run go vet on every package exactly once" || bad "$n shards: go vet does not cover every package exactly once"
  if grep -E ' test ' "$work/log-n" | grep -qv -- ' -race -count=1 '; then bad "$n shards: a go test without -race -count=1"; else ok "$n shards: every go test is -race -count=1"; fi
done
# Readers scope: only the readers, and sharded too.
: >"$work/log-d"
for i in 1 2; do run_stub "$work/log-d$i" --readers --shard "$i/2" && cat "$work/log-d$i" >>"$work/log-d"; done
check "--readers --shard 1/2 and 2/2 test exactly the readers" "$readers" "$(units_of_log "$work/log-d" test)"
# gofmt still runs on every shard (it is cheap), and a bad file fails it.
grep -q '^== gofmt' "$work/out" && ok "each run starts with gofmt" || bad "the run does not gofmt"
# A failing go fails the run.
if REAL_GO=$real_go STUB_LOG=/dev/null STUB_RC=1 PATH="$work/bin:$PATH" hack/test.sh --shard 1/2 >/dev/null 2>&1; then bad "a failing go passed the shard"; else ok "a failing go fails the shard"; fi

# ---- a new package appears ------------------------------------------------
# A scratch repository: the scripts, a table that knows one package, a
# readers file naming one, three packages and a tools/ module.
fx=$work/repo
mkdir -p "$fx/hack" "$fx/a" "$fx/b" "$fx/c/d" "$fx/tools/t"
cp hack/test.sh hack/shard.sh "$fx/hack/"
printf '. a 500\n' >"$fx/hack/test-durations.txt"
printf '. b\n' >"$fx/hack/test-readers.txt"
printf 'module example.com/fx\n\ngo 1.22\n' >"$fx/go.mod"
printf 'module example.com/fx/t\n\ngo 1.22\n' >"$fx/tools/t/go.mod"
for d in a b c/d; do printf 'package x\n' >"$fx/$d/x.go"; done
printf 'package main\n\nfunc main() {}\n' >"$fx/tools/t/main.go"
(cd "$fx" && git init -q && git add -A && git -c user.name=t -c user.email=t@t commit -qm fixture)
fx_all=$(cd "$fx" && hack/test.sh --list | sort)
check "fixture: the packages of both modules" $'. a\n. b\n. c/d\ntools/t .' "$fx_all"
for n in 1 2 3 4; do
  got=$(for i in $(seq 1 "$n"); do (cd "$fx" && hack/test.sh --list --shard "$i/$n" 2>/dev/null); done | sort)
  check "fixture, $n shards: each package, the new ones too, in exactly one" "$fx_all" "$got"
done
check "fixture: --readers selects the listed package" ". b" "$(cd "$fx" && hack/test.sh --list --readers)"
printf '. gone\n' >"$fx/hack/test-readers.txt"
if (cd "$fx" && hack/test.sh --list --readers >/dev/null 2>&1); then bad "fixture: a readers list naming no package passed"; else ok "fixture: a readers list naming no package of the tree fails"; fi

# ---- the wiring -----------------------------------------------------------
ci=.github/workflows/ci.yml
block=$(awk '$0 == "  test:" { c = 1; next } c && /^  [a-z0-9_-]+:[ ]*$/ { c = 0 } c' "$ci")
[ -n "$block" ] || { echo "FAIL $ci has no test job" >&2; exit 1; }
shards=$(sed -n 's/^ *shard: *\[\(.*\)\].*/\1/p' <<<"$block" | tr ',' '\n' | tr -d " '\"" | sed '/^$/d')
n=$(sed -n '$=' <<<"$shards")
n=${n:-0}
want_shards=$(seq 1 "$n" | sed "s#\$#/$n#")
if [ "$n" -lt 2 ]; then
  bad "$ci's test job has no matrix of at least 2 shards (shard: ['1/n', ...])"
elif [ "$(sort <<<"$shards")" != "$(sort <<<"$want_shards")" ]; then
  bad "$ci's test matrix is not every shard 1/$n..$n/$n exactly once: $(echo $shards)"
else
  ok "$ci's test matrix runs every shard of $n, once"
fi
grep -q 'fail-fast: false' <<<"$block" && ok "test's matrix does not stop at the first failing shard" || bad "test's matrix is fail-fast"
if grep -qF 'make test TEST_SHARD="$SHARD" TEST_SCOPE="$SCOPE"' <<<"$block" && grep -qF 'SHARD: ${{ matrix.shard }}' <<<"$block" &&
  grep -qF "SCOPE: \${{ needs.changes.outputs.test-scope == 'readers' && 'readers' || 'all' }}" <<<"$block"; then
  ok "the job passes its shard and the changes job's test-scope to make test"
else
  bad "test's run does not pass matrix.shard and the test-scope output to make test"
fi
grep -qF -- '--shard $(TEST_SHARD)' Makefile && grep -qF -- '--readers' Makefile && ok "make test takes TEST_SHARD and TEST_SCOPE" || bad "the Makefile's test target does not pass TEST_SHARD and TEST_SCOPE"

echo "test_test: $pass passed, $fail failed"
[ "$fail" = 0 ]
