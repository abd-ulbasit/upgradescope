#!/usr/bin/env bash
# Tests for hack/chart-survey.sh: the selection, error classification and
# aggregation steps against small fixtures, and the run's argument checks.
# Offline; needs bash and jq (and helm for the yaml2json case, which is
# skipped without it). Never contacts a cluster or the network.
set -euo pipefail
cd "$(dirname "$0")/.."
export KUBECONFIG=/dev/null LC_ALL=C

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pass=0 fail=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1" >&2; fail=$((fail + 1)); }
# check <name> <jq filter that must be true> <json file>
check() {
  if jq -e "$2" "$3" >/dev/null 2>&1; then ok "$1"; else
    bad "$1: $2 is not true of"
    sed 's/^/     /' "$3" >&2
  fi
}

# --- select ------------------------------------------------------------------
# Two search pages as the Artifact Hub search API returns them (stars order),
# and the package details the run fetches for each.
pkg() { # name repo url stars version app deprecated official verified
  jq -n --arg n "$1" --arg r "$2" --arg u "$3" --argjson s "$4" --arg v "$5" \
    --arg a "$6" --argjson d "$7" --argjson o "$8" --argjson vp "$9" \
    '{name: $n, stars: $s, version: $v, app_version: $a, deprecated: $d, official: $o,
      repository: {name: $r, url: $u, kind: 0, official: false, cncf: false, verified_publisher: $vp}}'
}
detail() { # repo name version app type deprecated kubeVersion
  mkdir -p "$work/details/$1"
  jq -n --arg v "$3" --arg a "$4" --arg t "$5" --argjson d "$6" --arg k "$7" \
    '{version: $v, app_version: $a, deprecated: $d, data: {type: $t, kubeVersion: $k}}' \
    >"$work/details/$1/$2.json"
}
{
  pkg alpha r1 https://r1.example/charts 100 1.0.0 2.0.0 false true true
  pkg beta r2 https://r2.example 90 0.1.0 0.1.0 false false false
  pkg alpha r1-oci oci://ghcr.io/r1/alpha 80 1.0.0 2.0.0 false false false
  pkg gamma r3 https://r3.example 70 3.0.0 '' false false true
  pkg alpha r4 https://r4.example 60 9.9.9 2.0.0 false false false
} | jq -s '{packages: .}' >"$work/page-0.json"
{
  pkg delta r5 oci://registry.example/charts/delta 50 4.0.0 4.1.0 false false false
  pkg eps r6 https://r6.example 40 5.0.0 5.0.0 true false false
  pkg zeta r7 https://r7.example 30 6.0.0 6.0.0 false false false
} | jq -s '{packages: .}' >"$work/page-1.json"
detail r1 alpha 1.0.0 2.0.0 application false '>=1.25.0-0'
detail r2 beta 0.1.0 0.1.0 application true '' # deprecated in its details only
detail r1-oci alpha 1.0.0 2.0.0 application false '>=1.25.0-0'
detail r3 gamma 3.0.0 '' library false ''
detail r4 alpha 9.9.9 2.0.0 '' false ''
detail r5 delta 4.1.0 4.1.0 application false '<1.37.0-0' # details are newer than search
detail r6 eps 5.0.0 5.0.0 application false ''
detail r7 zeta 6.0.0 6.0.0 application false ''
# r4's latest is a prerelease (by semver, though Artifact Hub does not flag
# it): the newest stable version is surveyed instead, with its appVersion.
jq '.version = "10.0.0-rc.1" | .available_versions = [
  {version: "10.0.0-rc.1", app_version: "3.0.0-rc.1", prerelease: false},
  {version: "9.9.9", app_version: "2.0.0", prerelease: false},
  {version: "9.10.0", app_version: "2.1.0", prerelease: false},
  {version: "9.11.0", app_version: "2.2.0", prerelease: true},
  {version: "9.2.0", app_version: "1.9.0", prerelease: false}]' "$work/details/r4/alpha.json" >"$work/x" && mv "$work/x" "$work/details/r4/alpha.json"

SURVEY_DETAILS_DIR=$work/details hack/chart-survey.sh select 3 "$work/page-0.json" "$work/page-1.json" \
  >"$work/sel.json"
check "select keeps the N most-starred eligible charts, in order" \
  '[.selected[] | "\(.repository)/\(.name)"] == ["r1/alpha", "r4/alpha", "r5/delta"]' "$work/sel.json"
check "select ranks from 1" '[.selected[].rank] == [1, 2, 3]' "$work/sel.json"
check "select takes version and appVersion from the package details" \
  '.selected[2].version == "4.1.0" and .selected[2].appVersion == "4.1.0"' "$work/sel.json"
check "select records stars, repository URL and kind, and flags" \
  '.selected[0] | .stars == 100 and .repoUrl == "https://r1.example/charts" and .repoKind == "http"
     and .official == true and .verifiedPublisher == true' "$work/sel.json"
check "select calls an oci:// repository oci" '.selected[2].repoKind == "oci"' "$work/sel.json"
check "select drops a package deprecated in its details" \
  'any(.dropped[]; .repository == "r2" and .name == "beta" and .reason == "deprecated")' "$work/sel.json"
check "select drops a library chart" \
  'any(.dropped[]; .repository == "r3" and .reason == "library chart")' "$work/sel.json"
check "select drops an exact duplicate, naming the more-starred copy" \
  'any(.dropped[]; .repository == "r1-oci" and .reason == "duplicate of r1/alpha")' "$work/sel.json"
check "select keeps a same-named chart that is not a duplicate" \
  'any(.selected[]; .repository == "r4")' "$work/sel.json"
check "select skips a prerelease latest for the newest stable version" \
  '.selected[1] | .version == "9.10.0" and .appVersion == "2.1.0" and .latestVersion == "10.0.0-rc.1"' "$work/sel.json"
check "select records the latest version as the surveyed one when it is stable" \
  '.selected[0] | .version == "1.0.0" and .latestVersion == "1.0.0"' "$work/sel.json"
check "select logs only the drops above the cutoff" \
  '(.dropped | length) == 3 and all(.dropped[]; .name != "eps")' "$work/sel.json"

SURVEY_DETAILS_DIR=$work/details hack/chart-survey.sh select 10 "$work/page-0.json" "$work/page-1.json" \
  >"$work/sel10.json"
check "select returns what there is when the pages run out" \
  '(.selected | length) == 4 and any(.dropped[]; .name == "eps" and .reason == "deprecated")' "$work/sel10.json"

rm "$work/details/r7/zeta.json"
if SURVEY_DETAILS_DIR=$work/details hack/chart-survey.sh select 10 "$work/page-0.json" "$work/page-1.json" \
  >"$work/out" 2>&1; then
  bad "select fails when a package's details are missing"
elif grep -q "r7/zeta" "$work/out"; then ok "select fails when a package's details are missing"; else
  bad "select names the package whose details are missing"
fi

# --- classify ----------------------------------------------------------------
classify_is() { # name want stderr
  printf '%s\n' "$3" >"$work/err"
  local got
  got=$(hack/chart-survey.sh classify "$work/err")
  if [ "$got" = "$2" ]; then ok "classify: $1"; else bad "classify: $1: got '$got', want '$2'"; fi
}
classify_is "a kubeVersion refusal" kubeVersion \
  'Error: chart requires kubeVersion: >=1.19.0 <1.37.0 which is incompatible with Kubernetes v1.37.0'
classify_is "a required() call" required-value \
  'Error: execution error at (gitlab/templates/NOTES.txt:72:4): certmanager-issuer.email is required'
classify_is "a values schema missing property" required-value \
  "Error: values don't meet the specifications of the schema(s) in the following chart(s):
x:
- at '': missing property 'adminPassword'"
classify_is "a fail() asking to define a value" required-value \
  'Error: execution error at (loki/templates/write/statefulset-write.yaml:50:28): Please define loki.storage.bucketNames.chunks'
classify_is "a fail() refusing to install without a value" required-value \
  'Error: execution error at (aws-load-balancer-controller/templates/deployment.yaml:67:28): Chart cannot be installed without a valid clusterName!'
classify_is "a schema type error is not a missing value" other \
  "Error: values don't meet the specifications of the schema(s) in the following chart(s):
external-dns:
- at '/policy': got null, want string"
classify_is "anything else" other 'Error: template: x/templates/a.yaml:3:9: executing "x" at <.Values.a.b>: nil pointer'

# --- aggregate ---------------------------------------------------------------
out=$work/out-dir
mkdir -p "$out/charts"
rec() { # dir json
  mkdir -p "$out/charts/$1"
  printf '%s\n' "$2" >"$out/charts/$1/record.json"
}
base='{"repository":"r","repoUrl":"https://r.example","repoKind":"http","stars":1,"official":false,
  "verifiedPublisher":false,"appVersion":"1","chart":{"type":"application","kubeVersion":""},
  "fetch":{"ok":true,"error":null},"render":{"ok":true,"class":null,"error":null,"prev":null},
  "scanPrev":{"target":"1.36","ok":true,"verdict":"ready","score":100,"blockers":0},"scanOfPrevRender":null}'
f() { # category severity key title [file]
  jq -n --arg c "$1" --arg s "$2" --arg k "$3" --arg t "$4" --arg file "${5:-x/templates/a.yaml}" \
    '{category: $c, severity: $s, key: $k, title: $t, objects: [{file: $file, line: 1, name: "o"}], objectsTotal: 1}'
}
scan() { # verdict findings-json-array
  jq -n --arg v "$1" --argjson f "$2" \
    '{ok: true, error: null, verdict: $v, score: 50, findings: $f, notAssessed: ["helm"], unrecognizedImages: []}'
}
mk() { # rank name scan-json [overrides-json]
  local o=${4:-}
  [ -n "$o" ] || o="{}"
  jq -n --argjson b "$base" --argjson r "$1" --arg n "$2" --argjson s "$3" --argjson o "$o" \
    '$b + {rank: $r, name: $n, version: "1.\($r).0", scan: $s} + $o'
}
psp=$(f removed-api blocker removed-api/policy/v1beta1/PodSecurityPolicy 'policy/v1beta1 PodSecurityPolicy removed in 1.25 (1 object)' c1/templates/psp.yaml)
dep=$(f deprecated-api warning deprecated-api/flowcontrol/v1beta3/FlowSchema 'flowcontrol v1beta3 deprecated')
nginx=$(f eol-addon blocker eol-addon/ingress-nginx 'Ingress NGINX Controller is end-of-life since 2026-03-24' c2/templates/controller-deployment.yaml)
nodata=$(f addon-no-data info addon-no-data/redis 'redis 7.0: no lifecycle data')
crd=$(f crd-version blocker crd-version/unserved/x.io/v1alpha1/Thing 'x.io/v1alpha1 Thing is not served')
inc=$(f chart-incompat blocker chart-incompat/istio 'istio 1.20 does not support Kubernetes 1.37')
inc=$(jq -c 'del(.objects[]) | .objectsTotal = 0' <<<"$inc") # add-on findings list no object
rec 01-r-c1 "$(mk 1 c1 "$(scan blocked "[$psp,$dep]")")"
rec 02-r-c2 "$(mk 2 c2 "$(scan blocked "[$nginx,$nodata]" | jq -c '.unrecognizedImages = ["docker.io/bitnami/redis", "quay.io/x/y"]')")"
rec 03-r-c3 "$(mk 3 c3 "$(scan ready "[$dep]")")"
rec 04-r-c4 "$(mk 4 c4 "$(scan ready '[]' | jq -c '.unrecognizedImages = ["quay.io/x/y"]')")"
rec 05-r-c5 "$(mk 5 c5 null '{"chart":{"type":"application","kubeVersion":">=1.20.0-0 <1.37.0-0"},
  "render":{"ok":false,"class":"kubeVersion","error":"incompatible with Kubernetes v1.37.0",
  "prev":{"kubeVersion":"1.36.0","ok":true,"class":null}},"scanPrev":null,
  "scanOfPrevRender":{"ok":true,"verdict":"ready","score":100,"findings":[]}}')"
rec 06-r-c6 "$(mk 6 c6 null '{"render":{"ok":false,"class":"required-value","error":"x is required","prev":null},"scanPrev":null}')"
rec 07-r-c7 "$(mk 7 c7 null '{"chart":null,"fetch":{"ok":false,"error":"404"},"render":null,"scanPrev":null}')"
rec 08-r-c8 "$(mk 8 c8 "$(scan blocked "[$crd,$inc]")")"
rec 10-r-c10 "$(mk 10 c10 '{"ok":false,"error":"boom","verdict":null,"score":null,"findings":[],"notAssessed":[],"unrecognizedImages":[]}')"
jq -n '{surveyDate: "2026-10-12", n: 9, target: "1.37", prevTarget: "1.36"}' >"$out/meta.json"
jq -n '{selected: [], dropped: [{name: "beta", repository: "r2", stars: 90, reason: "deprecated"}]}' >"$out/selection.json"

hack/chart-survey.sh aggregate "$out" >/dev/null
r=$out/results.json
check "aggregate counts every chart" '.summary.charts == 9 and (.charts | length) == 9' "$r"
check "aggregate keeps the charts in rank order" '[.charts[].rank] == [1, 2, 3, 4, 5, 6, 7, 8, 10]' "$r"
check "aggregate carries meta and the drop log" \
  '.meta.surveyDate == "2026-10-12" and .dropped[0].reason == "deprecated"' "$r"
check "aggregate: removed API" '.summary.classes.removedApi == 1' "$r"
check "aggregate: end-of-life add-on" '.summary.classes.eolAddon == 1' "$r"
check "aggregate: incompatible add-on" '.summary.classes.incompatibleAddon == 1' "$r"
check "aggregate: unserved CR version" '.summary.classes.unservedCrVersion == 1' "$r"
check "aggregate: any blocker" '.summary.classes.anyBlocker == 3' "$r"
check "aggregate: deprecated-only" '.summary.classes.deprecatedOnly == 1' "$r"
check "aggregate: kubeVersion excludes the target, and how many take the minor before" \
  '.summary.classes.kubeVersionExcludesTarget == 1 and .summary.classes.kubeVersionAcceptsPrev == 1' "$r"
check "aggregate: render failure, by class" \
  '.summary.classes.renderFailed == 1 and .summary.renderFailures == {"required-value": 1}' "$r"
check "aggregate: fetch failure" '.summary.classes.fetchFailed == 1' "$r"
check "aggregate: scan failure" '.summary.classes.scanFailed == 1' "$r"
check "aggregate: none of the above" '.summary.classes.none == 1' "$r"
check "aggregate: verdicts of the scanned charts" \
  '.summary.verdicts == {"blocked": 3, "ready": 2}' "$r"
check "aggregate: top findings count charts per key" \
  '.summary.topFindings[0] | .key == "deprecated-api/flowcontrol/v1beta3/FlowSchema" and .charts == 2' "$r"
check "aggregate: add-ons matched, by id and category" \
  '.summary.addons == [{"id":"ingress-nginx","category":"eol-addon","charts":1},
     {"id":"istio","category":"chart-incompat","charts":1},{"id":"redis","category":"addon-no-data","charts":1}]' "$r"
check "aggregate: the blocker list names chart, finding and file" \
  'any(.summary.blockerList[]; .chart == "c1" and .version == "1.1.0"
     and .category == "removed-api" and .file == "c1/templates/psp.yaml")
   and (.summary.blockerList | length) == 4' "$r"
check "aggregate: image repositories no add-on matcher recognised" \
  '.summary.unrecognizedImages == {charts: 2, distinct: 2,
     top: [{image: "quay.io/x/y", charts: 2}, {image: "docker.io/bitnami/redis", charts: 1}]}' "$r"
check "aggregate: the minor before, from the same charts" '.summary.prevVerdicts == {"ready": 6}' "$r"

csv=$out/results.csv
if [ "$(wc -l <"$csv" | tr -d ' ')" = 10 ]; then ok "aggregate: results.csv has a header and a row per chart"; else
  bad "aggregate: results.csv has $(wc -l <"$csv") lines, want 10"
fi
if head -1 "$csv" | grep -q '^rank,repository,name,version,'; then ok "aggregate: results.csv header"; else
  bad "aggregate: results.csv header: $(head -1 "$csv")"
fi
if grep -q '^5,"r","c5","1.5.0",.*"kubeVersion"' "$csv"; then ok "aggregate: results.csv row carries the render class"; else
  bad "aggregate: results.csv row 5: $(grep '^5,' "$csv")"
fi
for want in '| Removed API (blocker) | 1 |' '| `kubeVersion` excludes 1.37 | 1 |' '| None of the above | 1 |' \
  '| c1 | 1.1.0 | removed-api | policy/v1beta1 PodSecurityPolicy removed in 1.25 (1 object) | `c1/templates/psp.yaml` |' \
  '| c8 | 1.8.0 | chart-incompat | istio 1.20 does not support Kubernetes 1.37 | - |'; do
  if grep -qF -- "$want" "$out/tables.md"; then ok "aggregate: tables.md has '$want'"; else
    bad "aggregate: tables.md lacks '$want'"
    sed 's/^/     /' "$out/tables.md" >&2
  fi
done

# --- yaml2json ---------------------------------------------------------------
if command -v helm >/dev/null; then
  printf 'apiVersion: v2\nname: x\nkubeVersion: ">=1.21.0-0"\ndependencies:\n- name: common\n  version: 2.x\n' >"$work/c.yaml"
  hack/chart-survey.sh yaml2json "$work/c.yaml" >"$work/c.json"
  check "yaml2json converts helm show chart output" \
    '.kubeVersion == ">=1.21.0-0" and .dependencies[0].name == "common"' "$work/c.json"
else
  echo "skip yaml2json: no helm"
fi

# --- run argument checks (all before any network use) ------------------------
expect_run() { # name want-exit needle args...
  local name=$1 want=$2 needle=$3 got=0
  shift 3
  hack/chart-survey.sh run "$@" >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then ok "run: $name"; else
    bad "run: $name: exit $got (want $want)"
    sed 's/^/     /' "$work/out" >&2
  fi
}
expect_run "needs an output dir" 2 usage
expect_run "N must be a positive number" 2 "N must be" "$work/o" ten
expect_run "the target must be a minor" 2 "target must be" "$work/o" 5 1.37.0
expect_run "a missing upgradescope binary is a harness failure" 1 "not executable" "$work/o" 5 1.37 "$work/nope"
if hack/chart-survey.sh bogus >"$work/out" 2>&1; then bad "an unknown command fails"; else ok "an unknown command fails"; fi

echo "chart-survey_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
