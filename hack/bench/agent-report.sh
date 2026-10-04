#!/usr/bin/env bash
# agent-report.sh <results.jsonl> — the tables of hack/bench/agent.sh from its
# per-tick results: one line per fill level (the median of its ticks after the
# first, the mean of the two middle ones when their count is even, with the first tick, the cold one, beside it), then the requests by
# verb and resource at the last level. Needs jq. Knob: BENCH_REPORT_FORMAT=json
# prints the summary as JSON instead.
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
def levels: group_by(.label) | map(sort_by(.tick)) | sort_by(.[0].nodes);
def steady: if length > 1 then .[1:] else . end;
def get(res): (.byVerbResource | map(select(.verb == "GET" and .resource == res) | .count) | add) // 0;
def lst(res): (.byVerbResource | map(select(.verb == "LIST" and .resource == res) | .count) | add) // 0;
def summary: {
  fill: (.[0].label | capture("fill=(?<f>[^ ]+)").f),
  label: .[0].label,
  ticks: length,
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
  errors: (map(select(.error != null and .error != "")) | length)
};'

if [ "${BENCH_REPORT_FORMAT:-}" = json ]; then
  jq -s "$defs levels | map(summary)" "$1"
  exit 0
fi

echo
echo "Per tick, by fill level (medians over the ticks after the first, the mean of the middle two when their count is even, except peak heap, which is the maximum of them: the peak of live heap objects while a tick ran; RSS is the process peak so far):"
echo
echo "| Fill | Nodes | Helm releases | Requests | LIST pods | GET secrets | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB | First tick: requests, response MiB, wall s, CPU s |"
echo "|---|---|---|---|---|---|---|---|---|---|---|---|---|"
jq -rs "$defs"'
  levels | map(summary)[] |
  "| \(.fill) | \(.nodes) | \(.helmReleases) | \(.requests) | \(.listPods) | \(.getSecrets) | \(.bodyMiB) | \(.wireMiB) | \(.wallS) | \(.cpuS) | \(.peakHeapMiB) | \(.maxRssMiB) | \(.firstTick.requests), \(.firstTick.bodyMiB), \(.firstTick.wallS), \(.firstTick.cpuS) |"' "$1"

echo
echo "Requests by verb and resource at the last fill level (a steady tick):"
echo
echo "| Verb | Resource | Requests |"
echo "|---|---|---|"
jq -rs "$defs"'
  levels | last | steady | .[0] | .byVerbResource[] | "| \(.verb) | \(.resource) | \(.count) |"' "$1"
