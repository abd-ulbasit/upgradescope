#!/usr/bin/env bash
# pod-sample.sh — samples the agent pod's cgroup while it ticks (#229;
# docs/operations/scale.md, "The agent under the chart's CPU limit"): CPU
# used, CFS periods and throttled periods and time (cpu.stat), memory now and
# its peak, and the process's peak RSS (VmHWM), every few seconds, one TSV
# line per sample. A cgroup CPU quota, which is what the chart's
# agent.resources.limits.cpu is, throttles a process in ways GOMAXPROCS does
# not, and only the cgroup's own counters say by how much.
#
#   KUBECONFIG=/path/to/lab-kubeconfig NODE_CONTAINER=us-lab-137b-control-plane \
#     hack/bench/pod-sample.sh <namespace> <deployment> <seconds> > samples.tsv
#
# It reads the cluster named by KUBECONFIG only (required; the default
# kubeconfig is never used) and reads the node's cgroup files with
# `docker exec NODE_CONTAINER` (a kind node is a container; BENCH_DOCKER can
# prefix another way in, e.g. "ssh lab-host docker"). It needs kubectl and jq,
# and changes nothing.
set -euo pipefail

die() { echo "pod-sample: $*" >&2; exit 1; }
[ $# -eq 3 ] || die "usage: KUBECONFIG=<lab kubeconfig> NODE_CONTAINER=<kind node container> pod-sample.sh <namespace> <deployment> <seconds>"
[ -n "${KUBECONFIG:-}" ] || die "KUBECONFIG is required: the kubeconfig of the lab cluster (the default kubeconfig is never used)"
case "$KUBECONFIG" in *:*) die "KUBECONFIG must name a single file, not a list" ;; esac
[ -f "$KUBECONFIG" ] || die "KUBECONFIG=$KUBECONFIG is not a file"
[ "$(cd "$(dirname "$KUBECONFIG")" && pwd -P)/$(basename "$KUBECONFIG")" != "$(cd "$HOME/.kube" 2>/dev/null && pwd -P)/config" ] ||
  die "KUBECONFIG is ~/.kube/config; use the lab cluster's own kubeconfig file"
[ -n "${NODE_CONTAINER:-}" ] || die "NODE_CONTAINER is required: the container of the kind node the pod runs on"
ns=$1 deploy=$2 seconds=$3
case "$seconds" in "" | *[!0-9]*) die "seconds must be a whole number" ;; esac
docker_cmd=${BENCH_DOCKER:-docker}
kc() { kubectl --kubeconfig "$KUBECONFIG" "$@"; }

# The newest pod of the deployment that is running and not being deleted (a
# rollout leaves the old one terminating beside the new one).
pod=$(kc -n "$ns" get pods -o json | jq -r --arg d "$deploy" '[.items[] | select(.metadata.name | startswith($d + "-")) | select(.metadata.deletionTimestamp == null and .status.phase == "Running")] | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty')
[ -n "$pod" ] || die "no running pod of $deploy in $ns"
cid=$(kc -n "$ns" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].containerID}')
cid=${cid#containerd://}
[ -n "$cid" ] || die "pod $pod has no container id yet"
echo "pod-sample: $pod container $cid" >&2

# Runs on the node: one line of counters for the container's cgroup.
read -r -d '' remote <<EOF || true
cg=\$(find /sys/fs/cgroup -type d -name 'cri-containerd-$cid.scope' | head -1)
[ -n "\$cg" ] || exit 3
pid=\$(head -1 "\$cg/cgroup.procs")
get() { awk -v k="\$1" '\$1 == k { print \$2 }' "\$cg/cpu.stat"; }
printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "\$(get usage_usec)" "\$(get nr_periods)" "\$(get nr_throttled)" "\$(get throttled_usec)" \
  "\$(cat "\$cg/memory.current")" "\$(cat "\$cg/memory.peak" 2>/dev/null || echo 0)" "\$(awk '/VmHWM/ { print \$2 * 1024 }' /proc/\$pid/status)"
EOF

printf 'epoch_ms\tusage_usec\tnr_periods\tnr_throttled\tthrottled_usec\tmem_current\tmem_peak\trss_peak\n'
end=$((SECONDS + seconds))
while [ $SECONDS -lt $end ]; do
  now=$(($(date +%s) * 1000))
  # shellcheck disable=SC2086 # BENCH_DOCKER is a command prefix
  line=$($docker_cmd exec "$NODE_CONTAINER" sh -c "$remote" 2>/dev/null) || line=""
  [ -z "$line" ] || printf '%s\t%s\n' "$now" "$line"
  sleep "${BENCH_SAMPLE_INTERVAL:-5}"
done
