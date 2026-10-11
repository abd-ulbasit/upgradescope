#!/usr/bin/env bash
# The Helm chart survey (docs/research/helm-chart-survey-2026-10/): the N
# most-starred Helm charts on Artifact Hub, rendered at default values and
# scanned against a Kubernetes target.
#
#   hack/chart-survey.sh run <out-dir> [N] [target] [upgradescope]
#       N defaults to 50, target to 1.37, upgradescope to the one on PATH.
#       Writes <out-dir>/results.json, results.csv and tables.md, and keeps
#       every download, render, scan and error under <out-dir> to check by
#       hand. Downloads that are already in <out-dir> are reused, so start a
#       new survey in a new directory.
#   hack/chart-survey.sh select <N> <search-page.json>...
#       The selection step alone (SURVEY_DETAILS_DIR holds the package
#       details as <repository>/<name>.json): JSON {selected, dropped}.
#   hack/chart-survey.sh classify <helm-stderr-file>
#       kubeVersion, required-value or other.
#   hack/chart-survey.sh aggregate <out-dir>
#       Rebuilds results.json, results.csv and tables.md from the per-chart
#       records in <out-dir>/charts/*/record.json.
#   hack/chart-survey.sh yaml2json <file>
#       YAML to JSON with helm's own fromYaml (no yq needed).
#
# Method, for each selected chart:
#   1. select: Artifact Hub's search API, sorted by stars (Artifact Hub has
#      no install counts); deprecated packages, library charts and exact
#      duplicates (the same name, version and appVersion from another
#      repository; the more-starred copy is kept) are dropped and logged;
#   2. fetch: helm pull at the latest stable version (a latest version that
#      Artifact Hub flags as a prerelease, or that has a semver prerelease
#      suffix, gives way to the newest stable one), helm show chart;
#   3. render: helm template at default values with --kube-version
#      <target>.0, and again at the minor before;
#   4. scan: upgradescope scan --files of each render, at its own minor; a
#      chart whose kubeVersion refuses the target also has its render at the
#      minor before scanned against the target.
# A chart that cannot be fetched, rendered or scanned is data, recorded with
# its error: the run exits non-zero only on a harness failure (bad arguments,
# a missing tool, or the Artifact Hub API not answering).
#
# Only read-only downloads leave this machine: the Artifact Hub API, chart
# repositories and OCI registries. helm runs only `pull`, `show chart` and
# `template`, with its cache, config and data under <out-dir>/helm, and
# KUBECONFIG is /dev/null: no cluster is contacted.
# Tested offline by hack/chart-survey_test.sh. Needs bash, jq, curl, helm.
set -euo pipefail
export KUBECONFIG=/dev/null LC_ALL=C
self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"

ARTIFACTHUB=${SURVEY_ARTIFACTHUB:-https://artifacthub.io}
PAGE=60 # the search API's largest page

usage() {
  echo "usage: $0 run|select|classify|aggregate|yaml2json ..." >&2
  sed -n '3,21p' "$self" | sed 's/^# \{0,1\}//' >&2
  exit 2
}
die() { echo "chart-survey: $*" >&2; exit 1; }

# --- select ------------------------------------------------------------------
cmd_select() {
  [ $# -ge 2 ] || usage
  local n=$1 details=${SURVEY_DETAILS_DIR:?SURVEY_DETAILS_DIR is required}
  shift
  local tmp
  tmp=$(mktemp)
  # One {pkg, detail} per search result, in the pages' (stars) order.
  local p repo name
  while IFS= read -r p; do
    repo=$(jq -r '.repository.name' <<<"$p")
    name=$(jq -r '.name' <<<"$p")
    [ -f "$details/$repo/$name.json" ] || { rm -f "$tmp"; die "no package details for $repo/$name"; }
    jq -c --argjson p "$p" '{pkg: $p, detail: .}' "$details/$repo/$name.json" >>"$tmp"
  done < <(jq -c '.packages[]' "$@")
  jq -s --argjson n "$n" '
    # A version is a prerelease when Artifact Hub flags it or semver says so
    # (a "-" suffix): Artifact Hub calls an unflagged 1.21.0-pre.3 the latest.
    def pre: (.prerelease // false) or ((.version | sub("\\+.*$"; "")) | test("-"));
    def semver: .version | ltrimstr("v") | sub("[-+].*$"; "") | split(".") | map(tonumber? // 0);
    def stable($d): if ($d | {version, prerelease} | pre) | not then {version: $d.version, app_version: $d.app_version}
      else ([($d.available_versions // [])[] | select(pre | not)] | max_by(semver)
        // {version: $d.version, app_version: $d.app_version}) end;
    reduce .[] as $e ({selected: [], dropped: [], seen: {}};
      if (.selected | length) >= $n then . else
        $e.pkg as $p | $e.detail as $d
        | ($d.version // $p.version) as $latest
        | (if $d.version then stable($d) else {version: $p.version, app_version: $p.app_version} end) as $st
        | $st.version as $v
        | ($st.app_version // $p.app_version // "") as $a
        | "\($p.repository.name)/\($p.name)" as $id
        | "\($p.name)|\($v)|\($a)" as $dk
        | {name: $p.name, repository: $p.repository.name, stars: $p.stars} as $log
        | if ($p.deprecated // false) or ($d.deprecated // false) then .dropped += [$log + {reason: "deprecated"}]
          elif ($d.data.type // "") == "library" then .dropped += [$log + {reason: "library chart"}]
          elif .seen[$dk] then .dropped += [$log + {reason: "duplicate of \(.seen[$dk])"}]
          else .seen[$dk] = $id
            | .selected += [{
                rank: ((.selected | length) + 1),
                name: $p.name,
                repository: $p.repository.name,
                repoUrl: $p.repository.url,
                repoKind: (if ($p.repository.url | startswith("oci://")) then "oci" else "http" end),
                stars: $p.stars,
                official: (($p.official // false) or ($p.repository.official // false)),
                verifiedPublisher: ($p.repository.verified_publisher // false),
                cncf: ($p.repository.cncf // false),
                packageId: $p.package_id,
                version: $v,
                latestVersion: $latest,
                appVersion: $a
              }]
          end
      end)
    | {selected, dropped}' "$tmp"
  rm -f "$tmp"
}

# --- classify ----------------------------------------------------------------
cmd_classify() {
  [ $# -eq 1 ] || usage
  if grep -qE 'incompatible with Kubernetes' "$1"; then
    echo kubeVersion
  elif grep -qiE 'is required|are required|must be (set|provided|specified|defined)|must (provide|set|specify|define)|missing propert|required value|please (set|provide|specify|define)|without a valid' "$1"; then
    echo required-value
  else
    echo other
  fi
}

# --- yaml2json ---------------------------------------------------------------
cmd_yaml2json() {
  [ $# -eq 1 ] || usage
  local d
  d=$(mktemp -d)
  mkdir -p "$d/c/templates"
  printf 'apiVersion: v2\nname: yaml2json\nversion: 0.0.0\n' >"$d/c/Chart.yaml"
  printf '{{ .Values.in | fromYaml | toJson }}\n' >"$d/c/templates/j.yaml"
  if ! helm template y "$d/c" --set-file "in=$1" --show-only templates/j.yaml >"$d/out" 2>"$d/err"; then
    cat "$d/err" >&2
    rm -rf "$d"
    die "yaml2json: helm template failed on $1"
  fi
  grep -vE '^(---|# Source:)' "$d/out" | jq -c 'if type == "object" and has("Error") then error("yaml2json: \(.Error)") else . end'
  rm -rf "$d"
}

# --- aggregate ---------------------------------------------------------------
cmd_aggregate() {
  [ $# -eq 1 ] || usage
  local out=$1
  [ -f "$out/meta.json" ] && [ -f "$out/selection.json" ] || die "aggregate: $out has no meta.json or selection.json"
  compgen -G "$out/charts/*/record.json" >/dev/null || die "aggregate: no $out/charts/*/record.json"
  jq -s 'sort_by(.rank)' "$out"/charts/*/record.json >"$out/.charts.json"
  jq -n --slurpfile m "$out/meta.json" --slurpfile s "$out/selection.json" --slurpfile c "$out/.charts.json" '
    def findings: (.scan.findings // []);
    def blockers: [findings[] | select(.severity == "blocker")];
    def scanned: .render.ok == true and .scan.ok == true;
    def has_blocker($cat): any(blockers[]; .category == $cat);
    def has_deprecated: any(findings[]; .category == "deprecated-api");
    def count(f): map(select(f)) | length;
    def tally(f): group_by(f) | map({key: (.[0] | f | tostring), value: length}) | from_entries;
    def addoncats: ["eol-addon", "eol-approaching", "chart-incompat", "addon-no-data"];
    def known: ["removed-api", "chart-incompat", "eol-addon", "crd-version"];
    $c[0] as $charts
    | {
        meta: $m[0],
        dropped: $s[0].dropped,
        summary: ($charts | {
          charts: length,
          classes: {
            removedApi: count(scanned and has_blocker("removed-api")),
            incompatibleAddon: count(scanned and has_blocker("chart-incompat")),
            eolAddon: count(scanned and has_blocker("eol-addon")),
            unservedCrVersion: count(scanned and has_blocker("crd-version")),
            otherBlocker: count(scanned and any(blockers[]; .category as $k | known | index($k) | not)),
            anyBlocker: count(scanned and (blockers | length) > 0),
            deprecatedOnly: count(scanned and (blockers | length) == 0 and has_deprecated),
            eolApproaching: count(scanned and any(findings[]; .category == "eol-approaching")),
            kubeVersionExcludesTarget: count(.render.class == "kubeVersion"),
            kubeVersionAcceptsPrev: count(.render.class == "kubeVersion" and .render.prev.ok == true),
            renderFailed: count(.fetch.ok == true and .render.ok == false and .render.class != "kubeVersion"),
            fetchFailed: count(.fetch.ok == false),
            scanFailed: count(.render.ok == true and .scan.ok != true),
            none: count(scanned and (blockers | length) == 0 and (has_deprecated | not))
          },
          renderFailures: (map(select(.fetch.ok == true and .render.ok == false and .render.class != "kubeVersion")) | tally(.render.class)),
          verdicts: (map(select(scanned)) | tally(.scan.verdict)),
          prevVerdicts: (map(select(.scanPrev.ok == true)) | tally(.scanPrev.verdict)),
          topFindings: ([.[] | select(scanned) | findings | unique_by(.key)[]]
            | group_by(.key)
            | map({key: .[0].key, category: .[0].category, severity: .[0].severity, title: .[0].title, charts: length})
            | sort_by(-.charts, .key) | .[:20]),
          addons: ([.[] | select(scanned) | [findings[] | select(.category as $k | addoncats | index($k))
                    | {id: (.key | split("/")[1]), category}] | unique[]]
            | group_by([.id, .category])
            | map(.[0] + {charts: length})
            | sort_by(-.charts, .id, .category)),
          blockerList: [.[] | select(scanned) | . as $r | blockers[]
            | {rank: $r.rank, chart: $r.name, repository: $r.repository, version: $r.version,
               category, key, title, file: (.objects[0].file // null), line: (.objects[0].line // null)}],
          refused: [.[] | select(.render.class == "kubeVersion")
            | {rank, chart: .name, repository, version, kubeVersion: .chart.kubeVersion,
               acceptsPrev: (.render.prev.ok == true),
               prevRenderVerdict: .scanOfPrevRender.verdict,
               prevRenderBlockers: ([(.scanOfPrevRender.findings // [])[] | select(.severity == "blocker") | .title])}]
        }),
        charts: $charts
      }' >"$out/results.json"
  rm -f "$out/.charts.json"

  jq -r '
    (["rank", "repository", "name", "version", "appVersion", "stars", "repoKind", "official", "verifiedPublisher",
     "chartType", "kubeVersion", "fetchOk", "renderOk", "renderClass", "acceptsPrev", "verdict", "score",
     "blockers", "warnings", "infos", "blockerCategories", "findingKeys", "addons", "prevVerdict", "prevBlockers"] | join(",")),
    (.charts[] | (.scan.findings // []) as $f | [
      .rank, .repository, .name, .version, .appVersion, .stars, .repoKind, .official, .verifiedPublisher,
      .chart.type, .chart.kubeVersion, .fetch.ok, .render.ok, .render.class,
      (if .render.class == "kubeVersion" then .render.prev.ok else null end),
      .scan.verdict, .scan.score,
      ([$f[] | select(.severity == "blocker")] | length),
      ([$f[] | select(.severity == "warning")] | length),
      ([$f[] | select(.severity == "info")] | length),
      ([$f[] | select(.severity == "blocker") | .category] | unique | join(";")),
      ([$f[] | .key] | join(";")),
      ([$f[] | select(.category | IN("eol-addon", "eol-approaching", "chart-incompat", "addon-no-data"))
        | .key | split("/")[1]] | unique | join(";")),
      .scanPrev.verdict, .scanPrev.blockers
    ] | @csv)' "$out/results.json" >"$out/results.csv"

  jq -r '
    def esc: tostring | gsub("\\|"; "\\|") | gsub("\n"; " ");
    def row(f): "| " + (f | map(esc) | join(" | ")) + " |";
    .meta.target as $t | .meta.prevTarget as $p | .summary as $s | .summary.classes as $c
    | "### Charts by class (\($s.charts) charts, target \($t))", "",
      "A chart can be in more than one blocker class.", "",
      "| Class | Charts |", "|---|---|",
      row(["Removed API (blocker)", $c.removedApi]),
      row(["Incompatible add-on (blocker)", $c.incompatibleAddon]),
      row(["End-of-life add-on (blocker)", $c.eolAddon]),
      row(["Custom resources at an unserved version (blocker)", $c.unservedCrVersion]),
      row(["Other blocker", $c.otherBlocker]),
      row(["Any blocker", $c.anyBlocker]),
      row(["Deprecated API only (no blocker)", $c.deprecatedOnly]),
      row(["Add-on approaching end of life (warning)", $c.eolApproaching]),
      row(["`kubeVersion` excludes \($t)", $c.kubeVersionExcludesTarget]),
      row(["... of which accept \($p)", $c.kubeVersionAcceptsPrev]),
      row(["Render failure at default values", $c.renderFailed]),
      row(["Fetch failure", $c.fetchFailed]),
      row(["Scan failure", $c.scanFailed]),
      row(["None of the above", $c.none]),
      "",
      "Render failures by class: " + (if ($s.renderFailures | length) == 0 then "none"
        else ($s.renderFailures | to_entries | map("\(.key) \(.value)") | join(", ")) end) + ".",
      "",
      "### Verdicts", "",
      "| Verdict | At \($t) (rendered at \($t).0) | At \($p) (rendered at \($p).0) |", "|---|---|---|",
      (([$s.verdicts, $s.prevVerdicts] | map(keys[]) | unique)[] as $v
        | row([$v, ($s.verdicts[$v] // 0), ($s.prevVerdicts[$v] // 0)])),
      "",
      "### Charts with a blocker at \($t)", "",
      "| Chart | Version | Category | Finding | Rendered file |", "|---|---|---|---|---|",
      ($s.blockerList[] | row([.chart, .version, .category, .title, (if .file then "`\(.file)`" else "-" end)])),
      "",
      "### Charts whose `kubeVersion` excludes \($t)", "",
      "| Chart | Version | `kubeVersion` | Accepts \($p) | \($p) render scanned at \($t) |", "|---|---|---|---|---|",
      ($s.refused[] | row([.chart, .version, "`\(.kubeVersion)`", (if .acceptsPrev then "yes" else "no" end),
        ((.prevRenderVerdict // "not scanned") + (if (.prevRenderBlockers | length) > 0 then ": " + (.prevRenderBlockers | join("; ")) else "" end))])),
      "",
      "### Most common findings", "",
      "| Finding (first title seen) | Category | Severity | Charts |", "|---|---|---|---|",
      ($s.topFindings[] | row([.title, .category, .severity, .charts])),
      "",
      "### Add-ons matched to the registry (from findings)", "",
      "| Add-on | Finding category | Charts |", "|---|---|---|",
      ($s.addons[] | row([.id, .category, .charts])),
      "",
      "### Every chart", "",
      "| # | Chart | Repository | Version | Stars | Render at \($t).0 | Verdict at \($t) | Blockers | Warnings | Verdict at \($p) |",
      "|---|---|---|---|---|---|---|---|---|---|",
      (.charts[] | (.scan.findings // []) as $f
        | row([.rank, .name, .repository, .version, .stars,
          (if .fetch.ok == false then "fetch failed" elif .render.ok then "ok" else "failed (\(.render.class))" end),
          (.scan.verdict // "-"),
          ([$f[] | select(.severity == "blocker")] | length),
          ([$f[] | select(.severity == "warning")] | length),
          (.scanPrev.verdict // "-")]))
  ' "$out/results.json" >"$out/tables.md"
  echo "chart-survey: wrote $out/results.json, results.csv and tables.md"
}

# --- run ---------------------------------------------------------------------
get() { # url file: a harness failure if Artifact Hub does not answer
  curl -fsSL --retry 3 --retry-delay 2 -o "$2.tmp" "$1" || die "cannot fetch $1"
  mv "$2.tmp" "$2"
}

# trim <file>: the error to record, the first "Error:" line or else the last
# line, with the output directory replaced so no local path is recorded.
trim() {
  local e
  e=$( (grep -m1 '^Error' "$1" || tail -n1 "$1") | cut -c1-600)
  printf '%s' "${e//$out/<out>}"
}

render() { # tgz kube-version dest errfile
  rm -rf "$3"
  helm template survey "$1" --namespace survey --include-crds --kube-version "$2" --output-dir "$3" >/dev/null 2>"$4"
}

# scan <dir> <target> <json> <err>: the trimmed scan, or {ok: false, error}.
scan() {
  if "$bin" scan --files "$1" --target "$2" --output json --fail-on never >"$3" 2>"$4"; then
    jq -c '{ok: true, error: null, verdict, score, toolVersion, kbVersion,
      findings: [.findings[] | {category, severity, key, title, detail,
        objects: [(.objects // [])[:3][] | {file, line, name, namespace}],
        objectsTotal: (((.objects // []) | length) + (.objectsOmitted // 0))}],
      notAssessed: [(.notAssessed // [])[] | .capability],
      unrecognizedImages: (.unrecognizedImages // [])}' "$3"
  else
    jq -cn --arg e "$(trim "$4")" '{ok: false, error: $e, verdict: null, score: null, findings: [], notAssessed: [], unrecognizedImages: []}'
  fi
}

survey_chart() {
  local c=$1 rank name repo url kind version dir tgz
  IFS=$'\t' read -r rank name repo url kind version < <(jq -r '[.rank, .name, .repository, .repoUrl, .repoKind, .version] | @tsv' <<<"$c")
  dir=$out/charts/$(printf '%02d' "$rank")-$repo-$name
  echo "chart-survey: [$rank/$n] $repo/$name $version" >&2
  rm -rf "$dir"
  mkdir -p "$dir/pull"

  local fetch='{"ok":true,"error":null}' chart=null attempt
  for attempt in 1 2; do
    if [ "$kind" = oci ]; then
      helm pull "$url" --version "$version" -d "$dir/pull" >/dev/null 2>"$dir/pull.err" && break
    else
      helm pull --repo "$url" "$name" --version "$version" -d "$dir/pull" >/dev/null 2>"$dir/pull.err" && break
    fi
    [ "$attempt" = 2 ] || sleep 3
  done
  tgz=$(find "$dir/pull" -name '*.tgz' | head -n1)
  if [ -z "$tgz" ]; then
    fetch=$(jq -cn --arg e "$(trim "$dir/pull.err")" '{ok: false, error: $e}')
    jq -n --argjson c "$c" --argjson f "$fetch" \
      '$c + {chart: null, fetch: $f, render: null, renderPrev: null, scan: null, scanPrev: null, scanOfPrevRender: null}' \
      >"$dir/record.json"
    return 0
  fi
  helm show chart "$tgz" >"$dir/Chart.yaml"
  chart=$("$self" yaml2json "$dir/Chart.yaml" | jq -c '{apiVersion, type: (.type // "application"),
    appVersion, kubeVersion: (.kubeVersion // ""),
    dependencies: [(.dependencies // [])[] | {name, version, repository, condition}]}')

  local ok_t=false class_t=null ok_p=false class_p=null
  if render "$tgz" "$target.0" "$dir/rendered-$target" "$dir/render-$target.err"; then ok_t=true; else
    class_t=$("$self" classify "$dir/render-$target.err")
  fi
  if render "$tgz" "$prev.0" "$dir/rendered-$prev" "$dir/render-$prev.err"; then ok_p=true; else
    class_p=$("$self" classify "$dir/render-$prev.err")
  fi
  local rprev rtarget s=null sp=null sop=null
  rprev=$(jq -cn --arg kv "$prev.0" --argjson ok "$ok_p" --arg cl "$class_p" --arg e "$(trim "$dir/render-$prev.err")" \
    '{kubeVersion: $kv, ok: $ok, class: (if $ok then null else $cl end), error: (if $ok then null else $e end)}')
  rtarget=$(jq -cn --argjson ok "$ok_t" --arg cl "$class_t" --arg e "$(trim "$dir/render-$target.err")" --argjson p "$rprev" \
    '{ok: $ok, class: (if $ok then null else $cl end), error: (if $ok then null else $e end), prev: $p}')
  if [ "$ok_t" = true ]; then s=$(scan "$dir/rendered-$target" "$target" "$dir/scan-$target.json" "$dir/scan-$target.err"); fi
  if [ "$ok_p" = true ]; then
    sp=$(scan "$dir/rendered-$prev" "$prev" "$dir/scan-$prev.json" "$dir/scan-$prev.err" |
      jq -c --arg t "$prev" '{target: $t, ok, error, verdict, score,
        blockers: ([.findings[] | select(.severity == "blocker")] | length),
        warnings: ([.findings[] | select(.severity == "warning")] | length),
        blockerKeys: [.findings[] | select(.severity == "blocker") | .key]}')
    if [ "$class_t" = kubeVersion ]; then
      sop=$(scan "$dir/rendered-$prev" "$target" "$dir/scan-$prev-render-at-$target.json" "$dir/scan-$prev-render-at-$target.err")
    fi
  fi
  jq -n --argjson c "$c" --argjson ch "$chart" --argjson f "$fetch" --argjson r "$rtarget" \
    --argjson s "$s" --argjson sp "$sp" --argjson sop "$sop" \
    '$c + {chart: $ch, fetch: $f, render: $r, scan: $s, scanPrev: $sp, scanOfPrevRender: $sop}' >"$dir/record.json"
}

cmd_run() {
  [ $# -ge 1 ] || usage
  out=$1 n=${2:-50} target=${3:-1.37} bin=${4:-upgradescope}
  [[ $n =~ ^[1-9][0-9]*$ ]] || { echo "chart-survey: N must be a positive number, not '$n'" >&2; exit 2; }
  [[ $target =~ ^1\.([0-9]+)$ ]] || { echo "chart-survey: the target must be a minor such as 1.37, not '$target'" >&2; exit 2; }
  prev=1.$((BASH_REMATCH[1] - 1))
  case "$bin" in */*) ;; *) bin=$(command -v "$bin" || echo "$bin") ;; esac
  [ -x "$bin" ] && [ -f "$bin" ] || die "the upgradescope binary '$bin' is not executable"
  local t
  for t in helm jq curl; do command -v "$t" >/dev/null || die "$t is not on PATH"; done
  mkdir -p "$out"
  out=$(cd "$out" && pwd)
  bin=$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")
  export HELM_CACHE_HOME=$out/helm/cache HELM_CONFIG_HOME=$out/helm/config HELM_DATA_HOME=$out/helm/data

  # 1. select
  mkdir -p "$out/search" "$out/details"
  local page=0 got pages=() p repo name
  while :; do
    p=$out/search/page-$page.json
    [ -f "$p" ] || get "$ARTIFACTHUB/api/v1/packages/search?kind=0&sort=stars&limit=$PAGE&offset=$((page * PAGE))" "$p"
    pages+=("$p")
    while IFS=$'\t' read -r repo name; do
      [ -f "$out/details/$repo/$name.json" ] && continue
      mkdir -p "$out/details/$repo"
      get "$ARTIFACTHUB/api/v1/packages/helm/$repo/$name" "$out/details/$repo/$name.json"
      sleep 0.2
    done < <(jq -r '.packages[] | [.repository.name, .name] | @tsv' "$p")
    SURVEY_DETAILS_DIR=$out/details cmd_select "$n" "${pages[@]}" >"$out/selection.json"
    got=$(jq '.selected | length' "$out/selection.json")
    [ "$got" -ge "$n" ] && break
    [ "$(jq '.packages | length' "$p")" -lt "$PAGE" ] && break
    page=$((page + 1))
    [ "$page" -lt 20 ] || die "no $n eligible charts in the first $((20 * PAGE)) results"
  done
  echo "chart-survey: selected $got charts, dropped $(jq '.dropped | length' "$out/selection.json")" >&2

  # meta: what makes the run reproducible
  jq -n --arg date "${SURVEY_DATE:-$(date -u +%Y-%m-%d)}" --arg started "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson n "$n" --argjson got "$got" --arg t "$target" --arg p "$prev" \
    --arg us "$("$bin" version 2>&1)" --arg helm "$(helm version 2>&1)" \
    --arg jq "$(jq --version)" --argjson pages "${#pages[@]}" --arg ah "$ARTIFACTHUB" '
    {surveyDate: $date, startedAt: $started, n: $n, selected: $got, target: $t, prevTarget: $p,
     upgradescope: {versionOutput: $us,
       version: ($us | capture("^upgradescope (?<v>\\S+)").v // null),
       commit: ($us | capture("commit: +(?<c>\\S+)").c // null),
       kb: ($us | capture("kb: +(?<k>[^\n]+)").k // null)},
     helm: $helm, jq: $jq,
     artifactHub: {search: "\($ah)/api/v1/packages/search?kind=0&sort=stars&limit=60&offset=<60*page>",
       details: "\($ah)/api/v1/packages/helm/<repository>/<name>", pagesRead: $pages},
     commands: [
       "helm pull --repo <repoUrl> <name> --version <version> (http) | helm pull <repoUrl> --version <version> (oci)",
       "helm show chart <tgz>",
       "helm template survey <tgz> --namespace survey --include-crds --kube-version \($t).0 --output-dir rendered-\($t)",
       "helm template survey <tgz> --namespace survey --include-crds --kube-version \($p).0 --output-dir rendered-\($p)",
       "upgradescope scan --files rendered-\($t) --target \($t) --output json --fail-on never",
       "upgradescope scan --files rendered-\($p) --target \($p) --output json --fail-on never",
       "upgradescope scan --files rendered-\($p) --target \($t) --output json --fail-on never (only when kubeVersion refuses \($t))"
     ]}' >"$out/meta.json"

  # 2-4. fetch, render, scan
  rm -rf "$out/charts"
  mkdir -p "$out/charts"
  local c
  while IFS= read -r c; do survey_chart "$c"; done < <(jq -c '.selected[]' "$out/selection.json")

  # 5. aggregate
  cmd_aggregate "$out"
}

case "${1:-}" in
  run | select | classify | aggregate | yaml2json)
    cmd=$1
    shift
    "cmd_$cmd" "$@"
    ;;
  *) usage ;;
esac
