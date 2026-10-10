#!/usr/bin/env bash
# agent-report.sh <results.jsonl> — the tables of hack/bench/agent.sh from its
# per-tick results: one line per fill level (the median of its ticks after the
# first, the mean of the two middle ones when their count is even, with the first tick, the cold one, beside it), then the requests by
# verb and resource at the last level, with their response bytes and time, of
# a steady tick and of the first, and, for a run with the GitOps fill, the
# GitOps reads per level. A run with the agent's pod pass reuse (#228) has two
# kinds of tick after the first: those that listed every pod, which the first
# table and the breakdown report as before, and those that reused the last
# pass (addOnEvidenceAgeSeconds set), which get a table of their own. Needs jq.
# Knob: BENCH_REPORT_FORMAT=json prints the summary as JSON instead.
set -euo pipefail

[ $# -eq 1 ] && [ -s "$1" ] || { echo "usage: agent-report.sh <results.jsonl>" >&2; exit 2; }
command -v jq >/dev/null || { echo "agent-report: jq is required" >&2; exit 1; }

# The median of an array of numbers is the middle one, or for an even count
# the mean of the two middle ones (a default run has 4 steady ticks).
defs='
def median: sort | length as $n |
  if $n == 0 then null
  elif $n % 2 == 1 then .[($n - 1) / 2]
  else (.[$n / 2 - 1] + .[$n / 2]) / 2 end;
def mib: . / 1048576;
def r1: . * 10 | round / 10;
def r2: . * 100 | round / 100;
def levels: group_by(.label) | map(sort_by(.tick)) | sort_by(.[0].nodes);
def aged: (.addOnEvidenceAgeSeconds // 0) > 0;
# The ticks after the first (the first is the cold one).
def afterFirst: if length > 1 then .[1:] else . end;
# The ticks that reused the last pod pass, and the steady ticks that listed
# every pod: what every number but the second table is a median of. A run
# whose ticks after the first all reused the pass has no such tick, and
# falls back to them all.
def reused: afterFirst | map(select(aged));
def steady: (afterFirst | map(select(aged | not))) as $p | if ($p | length) > 0 then $p else afterFirst end;
def get(res): (.byVerbResource | map(select(.verb == "GET" and .resource == res) | .count) | add) // 0;
def lst(res): (.byVerbResource | map(select(.verb == "LIST" and .resource == res) | .count) | add) // 0;
# Requests and response bytes of every verb on one resource (the GitOps
# resources: applications, helmreleases, ocirepositories), in a tick.
def reqs(res): (.byVerbResource | map(select(.resource == res) | .count) | add) // 0;
def bytes(res): (.byVerbResource | map(select(.resource == res) | .bytes // 0) | add) // 0;
def gitops: ["applications", "helmreleases", "ocirepositories"] as $rs |
  (steady | map([$rs[] as $r | reqs($r)] | add) | median) as $req |
  (steady | map([$rs[] as $r | bytes($r)] | add) | median) as $b |
  {
    charts: (.[0].gitopsCharts // 0),
    applications: (steady | map(reqs("applications")) | median),
    applicationsMiB: (steady | map(bytes("applications")) | median | mib | r2),
    helmReleases: (steady | map(reqs("helmreleases")) | median),
    helmReleasesMiB: (steady | map(bytes("helmreleases")) | median | mib | r2),
    ociRepositories: (steady | map(reqs("ocirepositories")) | median),
    ociRepositoriesMiB: (steady | map(bytes("ocirepositories")) | median | mib | r2),
    requests: $req, bodyMiB: ($b | mib | r2),
    firstTickRequests: (.[0] | [$rs[] as $r | reqs($r)] | add)
  };
def reusedSummary: reused as $r | if ($r | length) == 0 then null else {
  ticks: ($r | length),
  requests: ($r | map(.requests) | median),
  listPods: ($r | map(lst("pods")) | median),
  bodyMiB: ($r | map(.bodyBytes) | median | mib | r1),
  wireMiB: ($r | map(.wireDownBytes + .wireUpBytes) | median | mib | r1),
  wallS: ($r | map(.wallMs) | median / 1000 | r1),
  cpuS: ($r | map(.cpuMs // 0) | median / 1000 | r1),
  peakHeapMiB: ($r | map(.peakHeapBytes) | max | mib | r1),
  maxAgeS: ($r | map(.addOnEvidenceAgeSeconds) | max)
} end;
def summary: {
  fill: (.[0].label | capture("fill=(?<f>[^ ]+)").f),
  label: .[0].label,
  ticks: length,
  steadyTicks: (steady | length),
  nodes: .[0].nodes, helmReleases: .[0].helmReleases,
  requests: (steady | map(.requests) | median),
  listPods: (steady | map(lst("pods")) | median),
  getSecrets: (steady | map(get("secrets")) | median),
  bodyMiB: (steady | map(.bodyBytes) | median | mib | r1),
  wireMiB: (steady | map(.wireDownBytes + .wireUpBytes) | median | mib | r1),
  wallS: (steady | map(.wallMs) | median / 1000 | r1),
  collectS: (steady | map(.collectMs) | median / 1000 | r1),
  cpuS: (steady | map(.cpuMs // 0) | median / 1000 | r1),
  peakHeapMiB: (steady | map(.peakHeapBytes) | max | mib | r1),
  maxRssMiB: (map(.maxRssBytes) | max | mib | r1),
  firstTick: (.[0] | {requests, wallS: (.wallMs / 1000 | r1), cpuS: ((.cpuMs // 0) / 1000 | r1), bodyMiB: (.bodyBytes | mib | r1), wireMiB: ((.wireDownBytes + .wireUpBytes) | mib | r1), peakHeapMiB: (.peakHeapBytes | mib | r1)}),
  errors: (map(select(.error != null and .error != "")) | length),
  reusedPods: reusedSummary,
  gitops: gitops
};
# The GitOps columns only when some level of the run read GitOps charts (the
# empty level has none, and still belongs in a table of a run that had them).
def withGitops: (any(.[]; .gitops.charts > 0)) as $any | map(if $any then . else .gitops = null end);'

if [ "${BENCH_REPORT_FORMAT:-}" = json ]; then
  jq -s "$defs levels | map(summary) | withGitops" "$1"
  exit 0
fi

echo
echo "Per tick, by fill level (medians over the ticks after the first that listed every pod, the mean of the middle two when their count is even, except peak heap, which is the maximum of them: the peak of live heap objects while a tick ran; RSS is the process peak so far):"
echo
echo "| Fill | Nodes | Helm releases | Requests | LIST pods | GET secrets | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB | First tick: requests, response MiB, wall s, CPU s |"
echo "|---|---|---|---|---|---|---|---|---|---|---|---|---|"
jq -rs "$defs"'
  levels | map(summary)[] |
  "| \(.fill) | \(.nodes) | \(.helmReleases) | \(.requests) | \(.listPods) | \(.getSecrets) | \(.bodyMiB) | \(.wireMiB) | \(.wallS) | \(.cpuS) | \(.peakHeapMiB) | \(.maxRssMiB) | \(.firstTick.requests), \(.firstTick.bodyMiB), \(.firstTick.wallS), \(.firstTick.cpuS) |"' "$1"

# The agent's pod pass reuse (#228): the ticks after the first that did not
# list the pods outside kube-system, from the same levels. Printed only when
# some tick reused the last pass.
if jq -es '[.[] | select((.addOnEvidenceAgeSeconds // 0) > 0)] | length > 0' "$1" >/dev/null; then
  echo
  echo "Per tick that reused the last pod pass instead of listing the pods outside kube-system, by fill level (medians over those ticks; peak heap is the maximum; age is the oldest evidence a tick reused):"
  echo
  echo "| Fill | Ticks | Requests | LIST pods | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Age of the evidence, s |"
  echo "|---|---|---|---|---|---|---|---|---|---|"
  jq -rs "$defs"'
    levels | map(summary) | map(select(.reusedPods != null))[] |
    "| \(.fill) | \(.reusedPods.ticks) | \(.reusedPods.requests) | \(.reusedPods.listPods) | \(.reusedPods.bodyMiB) | \(.reusedPods.wireMiB) | \(.reusedPods.wallS) | \(.reusedPods.cpuS) | \(.reusedPods.peakHeapMiB) | \(.reusedPods.maxAgeS) |"' "$1"
fi

# The GitOps fill (BENCH_GITOPS=1): what the lists of Argo CD Applications
# and Flux HelmReleases, and the reads of the OCIRepositories their chartRefs
# name (a list since #248, GETs where a list is refused), cost a steady tick. Printed only when the run had GitOps charts.
if jq -es '[.[] | select((.gitopsCharts // 0) > 0)] | length > 0' "$1" >/dev/null; then
  echo
  echo "GitOps reads per tick, by fill level (medians over the ticks after the first; response MiB are bodies as client-go read them, decompressed; the tick's own requests are in the table above):"
  echo
  echo "| Fill | GitOps charts read | LIST Applications: requests, MiB | LIST HelmReleases: requests, MiB | OCIRepositories (LIST or GET): requests, MiB | GitOps total: requests, MiB | First tick: GitOps requests |"
  echo "|---|---|---|---|---|---|---|"
  jq -rs "$defs"'
    levels | map(summary) | withGitops | .[] |
    "| \(.fill) | \(.gitops.charts) | \(.gitops.applications), \(.gitops.applicationsMiB) | \(.gitops.helmReleases), \(.gitops.helmReleasesMiB) | \(.gitops.ociRepositories), \(.gitops.ociRepositoriesMiB) | \(.gitops.requests), \(.gitops.bodyMiB) | \(.gitops.firstTickRequests) |"' "$1"
fi

# breakdown <jq selecting one tick>: its requests by verb and resource, with
# the response bytes client-go read for them and the time they took, summed
# over the requests (requests in flight at once each count their own; results
# recorded before the recorder kept bytes or time show -).
breakdown() {
  echo "| Verb | Resource | Requests | Response MiB | Time s (summed) |"
  echo "|---|---|---|---|---|"
  jq -rs "$defs"'
    def mibs: if . == null then "-" else (. / 1048576 * 100 | round / 100 | tostring) end;
    def secs: if . == null then "-" else (. / 1000 * 100 | round / 100 | tostring) end;
    '"$1"' | .byVerbResource[] | "| \(.verb) | \(.resource) | \(.count) | \(.bytes | mibs) | \(.ms | secs) |"' "$2"
}

echo
echo "Requests by verb and resource at the last fill level (a steady tick that listed every pod):"
echo
breakdown 'levels | last | steady | .[0]' "$1"

if jq -es '[.[] | select((.addOnEvidenceAgeSeconds // 0) > 0)] | length > 0' "$1" >/dev/null; then
  echo
  echo "Requests by verb and resource at the last fill level that reused the pod pass (a tick that reused it):"
  echo
  breakdown 'levels | map(select(reused | length > 0)) | last | reused | .[0]' "$1"
fi

echo
echo "Requests by verb and resource at the last fill level (the first tick, the cold one):"
echo
breakdown 'levels | last | .[0]' "$1"
