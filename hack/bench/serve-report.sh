#!/usr/bin/env bash
# serve-report.sh <results.jsonl> — the table of hack/bench/serve.sh from its
# per-round results. Needs jq.
set -euo pipefail

[ $# -eq 1 ] && [ -s "$1" ] || { echo "usage: serve-report.sh <results.jsonl>" >&2; exit 2; }
command -v jq >/dev/null || { echo "serve-report: jq is required" >&2; exit 1; }

echo
echo "| Backend | Pushers | Round | Pushes/s | p50 ms | p99 ms | Max ms | Retried (503 / conn errors) | CPU ms/push | Peak heap MiB | DB KiB/snapshot | DB total MiB |"
echo "|---|---|---|---|---|---|---|---|---|---|---|---|"
jq -r '
  def r1: . * 10 | round / 10;
  "| \(.backend)\(if .postgresRttMs then " (RTT \(.postgresRttMs | r1) ms)" else "" end) | \(.concurrency) | \(.round) | \(.pushesPerSecond | r1) | \(.p50Ms | r1) | \(.p99Ms | r1) | \(.maxMs | r1) | \(.retried) (\(.busy503) / \(.transportErrors)) | \(.cpuMsPerPush | r1) | \(.peakHeapMiB | round) | \(if .round == "duplicate" then "0" else (.dbBytesPerSnapshot / 1024 | r1) end) | \(.dbBytesAfter / 1048576 | r1) |"
' "$1"
