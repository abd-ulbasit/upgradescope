# Scale and cost

What the agent costs a Kubernetes API server on a large cluster, and what
one `serve` can take from a fleet. Every table here comes from the final
runs of one session on 4 October 2026, between 04:30 and 05:00 in UTC+5
(the agent "before" table at 04:32, the agent "after" table at 04:45, the
server table at 04:58; the raw result files are stamped in UTC, which is the
evening of 3 October). Two later sets of runs the same day, on main
`735751d` (times in UTC: 03:02 to 05:42), re-measured the agent tick without
and with Argo CD and Flux objects ([Re-measured on main](#re-measured-on-main-with-and-without-argo-cd-and-flux))
and ran the agent as a pod under three CPU limits ([CPU and the chart
limit](#cpu-and-the-chart-limit)). **Changes of the branch that makes the
cold Helm tick concurrent and the pod pass cheaper are not in any of these
numbers**; they are to be measured again when a release is cut. The harness
is in `hack/bench/`; you can run it against your own lab to get your own
numbers. Read [What is simulated](#what-is-simulated) before you quote
them.

## The short answers

- **A steady agent tick on a cluster of 2,001 nodes, about 14,000 pods and
  1,000 Helm releases made 53 API requests** and read 50 MiB of responses
  (3.4 MiB on the wire, compressed). It took 4.1 s and 3.0 CPU-seconds, with
  a peak live heap of 28 MiB and a peak RSS of 56 MiB (main `735751d`, 4
  October 2026; the earlier session's 61 requests, 5.6 s and 3.6
  CPU-seconds on `a3e72ea` are in the table below, and the 8 requests fewer
  are #232 reading each `kube-system` pod once). At the default interval of
  10 minutes that is 0.1 requests a second.
- **The first tick after the agent starts costs more**: it reads each Helm
  release once, 1,053 requests in all, 36.7 s and 23.5 CPU-seconds here.
  Before [#71](https://github.com/abd-ulbasit/upgradescope/issues/71) every
  tick cost that, because the agent fetched every release again each time:
  it was the one cost that grew with releases rather than with pages (1,000
  GETs of 1,061 requests).
- **Argo CD and Flux add a request per fifty objects and one per
  OCIRepository**: with 1,000 Applications and 1,000 HelmReleases (half of
  them with a `chartRef` to an OCIRepository of their own) a steady tick
  made 596 requests, 543 more, and read 63 MiB, 12.8 s and 6.2 CPU-seconds.
  See [the table](#re-measured-on-main-with-and-without-argo-cd-and-flux).
- **Memory fits the chart's defaults** (64Mi request, 256Mi limit) with room:
  the peak RSS of the benchmark never passed 61 MiB, and the agent as a pod
  reached 71 MiB (`VmHWM`), the first tick included. That is above the 64Mi
  request, which only informs scheduling.
- **The chart's default CPU limit was too low for such a cluster, and is now
  1 CPU.** As a pod at the old 200m the first tick gave up at its Helm
  step's deadline with 132 of 1,001 releases unread, and a steady tick took
  21 s instead of 4. See [CPU](#cpu-and-the-chart-limit).
- **One `serve` ingested 200 clusters with three targets each, all pushing at
  once, with no failed push and no retry**: 56 new snapshots a second on
  SQLite and 40 on Postgres, on a 2017 dual-core i3 that also ran the
  database. A real fleet of 200 clusters pushes at most 0.33 times a second
  (one change per cluster per 10-minute tick).

## What was measured, and on what

| | |
|---|---|
| Apiserver host | ThinkPad, Intel Core i3-7100U @ 2.40 GHz (2 cores, 4 threads, `/proc/cpuinfo`), 7,305 MiB RAM (`free -m`), Ubuntu 26.04.1 LTS, kernel 7.0.0-31, Docker 29.8.2, kind v0.33.0 |
| Cluster | kind, one control-plane node, Kubernetes 1.37.0 (`kindest/node:v1.37.0`), audit log at Metadata level on, KWOK v0.8.0 (controller and `fast` stages, pinned by version and sha256 in `hack/bench/agent.sh`) |
| Driver | MacBook Pro (`MacBookPro18,3`, Apple M1 Pro, 16 GiB, `sysctl hw.model`), Go 1.26.8, client-go 0.37.1. It seeded the cluster and cross-compiled the benchmarks; the measured agent ticks and the server benchmark ran on the ThinkPad |
| Postgres | 17.11 (`postgres:17-alpine`), a throwaway container on the ThinkPad's Docker engine, 0.1 ms round trip from the benchmark |
| SQLite | the embedded `modernc.org/sqlite` v1.60.1, in a temporary directory on the ThinkPad's SSD |
| upgradescope | the agent runs: main `a3e72ea` (#218, GitOps charts) plus this work; the "before" table is the same tree with the Helm step given no cache, which is what #71 changed. The server runs: main `f195ba5` plus this work (the server code is unchanged by this work; #217 is examples and tooling). The branch was rebased between the two, so its commit hashes name neither tree exactly. The numbers include #218's per-tick discovery and workload requests, but **not its Argo CD and Flux lists**: the lab of that session had neither tool's CRDs, so those lists were never made. The later runs measure them (see [Re-measured on main](#re-measured-on-main-with-and-without-argo-cd-and-flux)) |
| The later runs | main `735751d` for the agent, chart and collector (main has since moved to `a93ba31`, which changes `internal/engine` and `internal/suppress`, the evaluate step, and the server; not the collector, the agent or the chart's agent values: it edits a docs-link comment under `server.ingress` in `values.yaml`, and the chart README), with this branch's harness (`hack/bench/`, the GitOps fill, `pod-sample.sh`). The pod runs used a `linux/amd64` binary cross-compiled on the Mac with the same flags as `Dockerfile` (`CGO_ENABLED=0`, `-trimpath`, `-s -w`), packed with `Dockerfile.release` by `docker build` on the ThinkPad's engine and loaded with `kind load docker-image`: the dashboard bundle it embeds is the committed one, not a rebuilt one. Lab `us-lab-137b` (kind 1.37.0, a second cluster beside the first on the same ThinkPad), Argo CD Application CRD v3.5.3, Flux helm-controller CRDs v1.6.5 and source-controller v1.9.6 (OCIRepository only), pinned by sha256 in `hack/bench/agent.sh` |
| Also running | two other idle kind clusters on the same ThinkPad, and for the agent runs the lab's own KWOK controller keeping 2,000 nodes alive |
| Also running, later runs | the lab's KWOK controller, and a second lab that another session used on the same ThinkPad. Its control-plane container used 11 to 19% of one core and 565 to 945 MiB whenever it was sampled (`docker stats`, at each fill level and at the start and end of the pod runs): the idle figure of a kind control plane, not a 2,000-node fill, which takes 2.4 to 3.6 cores. The ThinkPad's load average was 2.4 at the start of the GitOps run, and 9 to 25 during its fills and the pod runs (4 threads), so the wall times of the later runs are noisier than the counts and CPU-seconds, and are given as ranges |

### What is simulated

- **The nodes are fake.** KWOK makes 2,000 `Node` objects report Ready with
  a kubelet version and marks the pods placed on them Running. No kubelet,
  container runtime or CNI exists, so the apiserver does none of the work
  real kubelets cause (heartbeats from 2,000 machines, status patches of
  real pods, watches). KWOK's own heartbeats keep the control-plane
  container busy: 2.4 to 3.6 cores and 2.9 to 3.0 GiB at full size (sampled
  once, at the end of the level).
- **There is one apiserver, one etcd member, and a kind control plane.** No
  HA control plane, no load balancer in front, no priority-and-fairness
  contention with other clients, no admission webhooks, and the agent talks
  to it from the same host (a round trip well under a millisecond). The
  requests of a tick are mostly sequential, so a slow link multiplies: a
  trial run from a laptop over a VPN (60 ms round trip) took 13 to 16 s for
  the tick that took 5.4 s beside the apiserver (250 releases), and an earlier
  trial beside the apiserver, with about 1,360 releases because of a seeding
  bug since fixed, took 40 to 78 s a tick and reached the Helm step's
  deadline on the first (13 releases unread).
- **The objects are generated.** ConfigMaps, zero-replica Deployments and
  pods with one container each; 1,000 `helm.sh/release.v1` Secrets in
  Helm's storage format (base64 of gzip of JSON), 80% small, 15% medium and
  5% large releases, 22.8 MB stored and 106.9 MB decompressed in all
  (compare the real charts measured in `internal/collect/helm.go`). Every
  node also carries two DaemonSet pods (`kube-proxy` and `kindnet`, created
  by the cluster's own DaemonSets), so the cluster holds about 4,000 pods
  more than the 10,000 seeded: 14,000 in all, 4,000 of them in `kube-system`.
  A production cluster's pods are more varied, so they compress less on the
  wire than these.
- **The fleet is generated too.** The server benchmark's 200 inventories are
  50% small (10 nodes), 40% medium (40 nodes) and 10% large (400 nodes), with
  Helm releases, add-ons and removed-API usage drawn from the shipped
  knowledge base; a push averages 28 KiB, 2 KiB gzipped. Its load generator
  runs in the server's process and talks to it over loopback.

## The agent

`hack/bench/agent.sh` grows the cluster in four steps (empty, a quarter,
half, all of it) and runs the agent's real tick, five times at each: collect,
evaluate, write the `ClusterReadiness` status. The clients are the ones the
agent builds, wrapped to count requests by verb and resource; a TCP forwarder
counts the bytes on the wire. Each row is the median of the ticks after the
first (with the default 5 ticks, 4: the mean of the two middle values; the
peak heap is the maximum of them); the first, which fills the Helm cache, is
beside it.

Full size is 2,001 nodes (2,000 fake and the control plane), 10,000 seeded
pods plus the 4,000 DaemonSet pods, 6,000 ConfigMaps, 4,000 Deployments, 100
namespaces and 1,000 Helm release Secrets.

**Steady tick, after the fix** (Helm releases fetched only when new or
changed; the first session's run, main `a3e72ea`, before #232 made each pod
be read once: [re-measured below](#re-measured-on-main-with-and-without-argo-cd-and-flux)):

| Fill | Nodes | Helm releases | Requests | Of them LIST pods | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB | First tick: requests, response MiB, wall s, CPU s |
|---|---|---|---|---|---|---|---|---|---|---|---|
| empty | 1 | 0 | 22 | 2 | 4.0 | 0.4 | 0.5 | 0.4 | 21.7 | 54.2 | 22, 4.0, 2.5, 0.4 |
| 1/4 | 501 | 250 | 29 | 11 | 19.8 | 1.5 | 1.3 | 1.2 | 23.6 | 58.5 | 279, 24.8, 5.6, 5.5 |
| 1/2 | 1,001 | 500 | 40 | 20 | 35.7 | 2.4 | 2.5 | 1.9 | 23.2 | 48.8 | 540, 46.8, 14.1, 12.2 |
| full | 2,001 | 1,000 | 61 | 38 | 67.7 | 4.3 | 5.6 | 3.6 | 27.1 | 54.4 | 1,061, 90.0, 36.1, 24.4 |

**The same ticks before the fix** (a GET per release on every tick). These
rows were measured with a local patch that gave the Helm step no cache. The
shipped `BENCH_NO_HELM_CACHE=1 make bench-agent KUBECONFIG=...` (the
benchmark's `UPGRADESCOPE_BENCH_NO_HELM_CACHE=1`) does the same, so you can
reproduce them; no published number was taken with it:

| Fill | Requests | GET Secrets | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB |
|---|---|---|---|---|---|---|---|---|
| empty | 22 | 0 | 4.1 | 0.4 | 0.5 | 0.4 | 22.0 | 54.5 |
| 1/4 | 279 | 250 | 25.0 | 6.3 | 5.7 | 5.5 | 21.2 | 47.9 |
| 1/2 | 540 | 500 | 47.1 | 12.8 | 12.4 | 11.7 | 25.2 | 48.4 |
| full | 1,061 | 1,000 | 90.3 | 25.1 | 32.9 | 23.5 | 27.9 | 55.3 |

"Response MiB" is what client-go read, decompressed (the typed client asks
for protobuf, which `TestNewClientsAskForProtobuf` pins). "Wire MiB" is what
crossed the connection, both ways, gzip and TLS included. The heap is the
peak of live heap objects while the tick ran; RSS is the process's peak so
far, so it only grows.

**The requests of a steady tick at full size**, by verb and resource:

| Verb | Resource | Requests |
|---|---|---|
| LIST | pods | 38 |
| LIST | nodes | 5 |
| LIST | secrets (metadata) | 3 |
| GET | API discovery | 4 |
| GET | `clusterreadinesses` | 2 |
| CREATE | `clusterreadinesses` | 1 |
| GET | `/metrics`, `/version`, `kube-system` namespace | 1 each |
| LIST | configmaps (metadata), CRDs, IngressClasses, namespaces | 1 each |
| UPDATE | `clusterreadinesses/status` | 1 |

### Re-measured on main, with and without Argo CD and Flux

Two runs of the same harness on 4 October 2026 (UTC), on main `735751d`, the
tick being the agent's own as in the tables above. The first is the cluster
at full size with no Argo CD or Flux CRD installed (`BENCH_STEPS="0 1"`,
5 ticks each, stamped 03:53 UTC). The second is `BENCH_GITOPS=1`: the three
upstream CRDs installed in the lab, and at each level Argo CD Applications
and Flux HelmReleases in proportion to the fill (1,000 of each at full size:
half of the Applications with `spec.sources`,
half of the HelmReleases with a `chartRef` to an OCIRepository of their own,
500 of those; every one deploys to the scanned cluster and every chart
resolves, so the collector reads 2,000 charts). The levels 0, 1/4 and 1/2
of the second run are one invocation (03:03 UTC). Seeding the full level in
that invocation failed (`etcdserver: request timed out`, the loaded lab's
etcd), so the full level is a second invocation of `BENCH_STEPS=1` that
continued the fill (03:18 UTC) and measured its 5 ticks; the seeder now
retries such errors. The levels below it were not measured again. The benchmark failed any
run in which the collector did not read back every seeded chart, and
accepted one gap: the helm capability is partial with only Argo CD skipped,
which is the documented gap for charts Argo CD renders without a release.

The agent, no GitOps objects (the CRDs absent):

| Fill | Nodes | Helm releases | Requests | Of them LIST pods | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB | First tick: requests, response MiB, wall s, CPU s |
|---|---|---|---|---|---|---|---|---|---|---|---|
| empty | 1 | 0 | 22 | 2 | 4.7 | 0.4 | 0.5 | 0.5 | 21.6 | 55.2 | 22, 4.6, 2.6, 0.5 |
| full | 2,001 | 1,000 | 53 | 30 | 50.4 | 3.4 | 4.1 | 3.0 | 28.5 | 55.8 | 1,053, 72.6, 36.7, 23.5 |

The four steady ticks at full size took 3.4 to 4.7 s and 2.8 to 3.2
CPU-seconds. The one request-count difference from the earlier table is
the 30 pod list requests against 38: #232.

The agent with Argo CD and Flux (1,000 Applications and 1,000 HelmReleases
at full size, the same number at a quarter and a half of it):

| Fill | Nodes | Helm releases | Applications | HelmReleases | Requests | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB | First tick: requests, response MiB, wall s, CPU s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| empty (CRDs, no objects) | 1 | 0 | 0 | 0 | 23 | 4.4 | 0.4 | 0.5 | 0.4 | 21.3 | 53.9 | 23, 4.3, 2.6, 0.5 |
| 1/4 | 501 | 250 | 250 | 250 | 165 | 19.0 | 1.8 | 2.4 | 1.8 | 25.8 | 49.7 | 415, 24.0, 7.4, 6.0 |
| 1/2 | 1,001 | 500 | 500 | 500 | 309 | 33.7 | 2.9 | 4.5 | 3.1 | 27.3 | 52.3 | 809, 44.8, 18.0, 12.8 |
| full | 2,001 | 1,000 | 1,000 | 1,000 | 596 | 63.2 | 5.4 | 12.8 | 6.2 | 31.6 | 60.8 | 1,596, 85.5, 44.4, 26.2 |

What the GitOps reads cost in those ticks (medians of the ticks after the
first; the first tick makes the same GitOps requests, they have no cache):

| Fill | Charts read | LIST Applications: requests, response MiB | LIST HelmReleases: requests, response MiB | GET OCIRepositories: requests, response MiB | GitOps total: requests, response MiB |
|---|---|---|---|---|---|
| empty | 0 | 1, 0 | 1, 0 | 0, 0 | 2, 0 |
| 1/4 | 500 | 5, 1.82 | 5, 0.91 | 125, 0.23 | 135, 2.96 |
| 1/2 | 1,000 | 10, 3.69 | 10, 1.83 | 250, 0.47 | 270, 5.98 |
| full | 2,000 | 20, 7.46 | 20, 3.65 | 500, 0.94 | 540, 12.05 |

- **Argo CD and Flux are 540 of the 596 requests** of a steady tick at full
  size: ceil(N / 50) for each list (20 for 1,000 objects) and one GET for each
  distinct OCIRepository (500), which are made one after another. Beside a
  tick that made 53, that is the largest request count the agent has. The
  tick took 12.8 s against 4.1 without them (two runs under different host
  load); over a link with a 60 ms round trip the 500 GETs alone would take
  30 s (computed, not measured).
- **The bytes are small beside the pods**: 12 MiB of the 63. A list response
  was 7.6 KiB per Application, 3.7 KiB per HelmRelease and 1.9 KiB per
  OCIRepository. These objects are generated (the seeder's own sizes are
  6.4, 2.6 and 1.1 KiB as JSON, which is 6,588, 2,681 and 1,083 bytes): an Application's status lists every object
  it manages (8 to 20 here) and a real one is often larger, so scale the
  bytes by the size of yours; the request counts do not depend on it.
- **The tick gained 3.2 CPU-seconds** (6.2 against 3.0), decoding the JSON.
- With `rbac.gitops.*` off, none of this is read: the agent lists nothing
  of Argo CD or Flux, and the helm capability is partial (naming the
  forbidden list) when the CRDs are served.
- The API discovery requests went from 4 to 7: the agent now asks which
  versions of the three groups are served, once each.

### What grows with what

Everything is linear; nothing was superlinear. Each list is paged at 500
objects, so a tick makes ceil(N / 500) requests per resource (counted on
`a3e72ea`, before #232 read each pod once, which made the pod lists 30
requests instead of 38): 38 are pod
requests (4,000 pods in `kube-system` first, for the version check, then 14,000 in all namespaces), 5 are
nodes, 3 are the metadata list of Helm Secrets. The rest is constant, 15
requests. Four of them are API discovery, two more than before #218, whose
GitOps detection asks the apiserver which API groups it serves every tick.
A cluster with no Helm release (the empty row: 22 requests) also lists
Deployments, StatefulSets and DaemonSets, metadata only, to look for the
tracking labels of Argo CD and Flux. Those three lists are 3 of the 22:
without them the row would be 2 + 1 + 1 + 15 = 19 (pod, node and Secret
lists, then the constant 15), and before #218's two extra discovery calls it
was 17. [Architecture](../architecture.md#api-cost-per-tick) states the
formula.

### CPU and the chart limit

The tick is CPU-bound in the agent: 3.0 CPU-seconds when steady and 23.5 on
the first tick at full size (the benchmark process also runs its byte
counter, a small share). The chart's limit for the agent
(`agent.resources.limits.cpu`) is a cgroup CPU quota, which no benchmark in
a test binary applies, so the agent was run as a pod: the chart installed
with `helm` into the lab (and nothing else), the image above, 2,001 nodes and
1,000 Helm releases (the lab held 1,001: the chart's own release is the extra
one) and no Argo CD or Flux objects, and the default interval
of 10 minutes, so that the tick timeout is 5 minutes and the Helm step, the
second of six, gets the share it gives in production (the agent's own
status says "gave up after 59s"). The limit was changed with
`helm upgrade --set agent.resources.limits.cpu=...`, which starts a new pod
and so a first tick with an empty cache. `hack/bench/pod-sample.sh` read the
container's `cpu.stat` (CFS periods, throttled periods and time) and
`memory.peak` from the kind node's cgroup every few seconds, and the
process's `VmHWM`; tick durations are from the agent's log and the Helm gap
from the `ClusterReadiness` status after each tick. The throttling is the
kernel's own count, not the kubelet's cAdvisor.

| Limit | Tick | Wall s | CPU s | Helm step | Throttled periods | Throttled s |
|---|---|---|---|---|---|---|
| 200m (old default) | first | 73.4 | 12.0 | **gave up at its deadline**: 869 of 1,001 read, 132 not (and the Argo CD and Flux discovery after it gave up too) | 511 of 689 (74%) | 37.1 |
| 200m | second | 105.5 | 13.0 | complete | 601 of 800 (75%) | 47.7 |
| 200m | steady | 21.7 | 2.8 | complete | 114 of 198 (58%) | 8.9 |
| 200m | steady | 21.1 | 2.8 | complete | 129 of 162 (80%) | 10.3 |
| 500m | first | 47.9 | 22.9 | complete | 394 of 496 (79%) | 18.5 |
| 500m | steady | 17.1 | 3.0 | complete | 36 of 119 (30%) | 1.7 |
| 500m | steady | 28.6 | 2.9 | complete | 33 of 136 (24%) | 1.5 |
| 1 CPU | first | 35.2 | 22.1 | complete | 18 of 369 (5%) | 0.1 |
| 1 CPU | steady | 6.9 | 2.8 | complete | 0 of 43 | 0.0 |

All on main `735751d`, 4 October 2026, 04:13 to 05:42 UTC; the benchmark
process outside a quota took 36.7 s and 23.5 CPU-seconds for the first tick
and 3.4 to 4.7 s and 2.8 to 3.2 CPU-seconds for a steady one (table above).
The peak RSS of the agent process was 66 to 71 MiB in every pod, and the
cgroup's peak memory (page cache included) 98 MiB at most, against the
256Mi limit.

- **At 200m the first tick did not finish its Helm step.** The tick used
  12.0 CPU-seconds in its 73 s, which is all 0.2 CPU gives, and left 132
  releases unread; the status said so (`helm (partial)`, the first release
  not read, "step deadline: gave up after 59s"). The following tick
  completed the step, in 105 s, using 13.0 CPU-seconds, more than 132
  releases explain (about 3 for them, 3 for the rest): the tick after a
  partial one seems to decode again some of what the partial one had
  decoded. That is not investigated here. A steady tick took 21 s against
  the 4 s it takes without a quota, throttled in 58 to 80% of the CFS
  periods it ran in.
- **At 500m the first tick finished, in 48 s, and so did its Helm step,
  inside the 59 s it is given** (the tick's own timeout is 5 minutes, and the
  Helm step is the second of its six). The step's own wall time was not
  logged, so the margin is not known; it is the margin of one run on a
  ThinkPad that was also doing other things, and the tick had Argo CD and
  Flux still to read, which share the Helm step's 59 s (they are not in this
  table): a cluster with more releases or a slower apiserver would give up
  here too. The steady
  ticks used 3.0 CPU-seconds and were throttled for only 1.5 to 1.7 s; their
  17 and 29 s are the apiserver and the host, whose load average was about 9
  when sampled after them (4 threads) and whose lab control plane was using
  1 to 3 cores for KWOK's heartbeats. Do not read a wall time of this
  table as the quota's.
- **At 1 CPU the first tick took 35 s, nearly what it takes with no quota,
  and was throttled in 5% of its periods.** A steady tick took 6.9 s. In
  this pod's life the control plane of the lab restarted once, at 05:19 UTC
  (the kubelet restarted the agent for a failed liveness probe and the
  apiserver had restarted; the cause was not investigated, and the host had
  290 MiB of memory free, with 2,000 fake nodes on it). The first tick after it
  (93 s, 700 of 1,001 releases read, none of its periods throttled) and
  the one after (27 s, 16.6 CPU-seconds) are the lab failing, not the
  quota, and are left out of the table; the first tick at 1 CPU is from
  before it, and the steady one from after.

**The chart's default is now 1 CPU** (`agent.resources.limits.cpu`),
requests unchanged at 50m. The old default could not read 1,000 Helm
releases in a first tick, and said so only in the report's list of what was
not assessed; 500m just did. For scheduling, a limit reserves nothing (the
request does), so a small cluster, whose tick uses well under one
core-second, is not charged for it. A namespace quota is different: a
ResourceQuota on `limits.cpu` counts the limit (800m more than before), and
a LimitRange whose max CPU is below 1 rejects it, so a `helm upgrade` on
default values can fail pod admission there; set `agent.resources.limits.cpu`
yourself in such a namespace (see [Upgrade](upgrade.md)).

How far 1 CPU goes is **computed from these runs, not measured**. The
Helm step is the wall time of the first tick less the steady tick that
follows (35.2 s less 6.9 s, about 28 s for 1,001 releases, one sequential
GET each), and it must finish within its 59 s: 59 / 28.3 x 1,001 is about
2,100, so "about 2,000 releases". It is not the 23.5 CPU-seconds per 1,000
releases of the first tick as a whole (that is CPU, not the step's wall
time, and the whole tick took 35 s). The Argo CD and Flux reads run inside
the same step and so share its 59 s: they added 8.7 s to a steady tick with
no quota (12.8 s against 4.1 s, [above](#re-measured-on-main-with-and-without-argo-cd-and-flux))
and 3.2 CPU-seconds, so with them the same arithmetic gives
(59 - 8.7) / 28.3 x 1,001, about 1,750 releases at 1,000 Applications and
1,000 HelmReleases. Both assume the step scales with the number of releases
and that a slower apiserver does not stretch it, and neither was run. Beyond
that, give the agent more CPU or a longer interval (`agent.interval`, whose
half is the tick timeout; the Helm step gets a fifth of what is left of it
after the first step, `runSteps`, so a longer interval also lengthens the
step's deadline). A
quota does not help the branch that makes the Helm step concurrent: that
spends the same CPU-seconds sooner, on more cores.

### Hotspots

| What | Found | Status |
|---|---|---|
| One GET per Helm release on every tick: 1,000 of 1,061 requests, 90 MiB, 33 s and 23.5 CPU-seconds at full size | The only cost that grew with releases, not pages | **Fixed**: the agent keeps what it decoded, keyed by the storage object's UID and resourceVersion; a steady tick makes none (61 requests, 5.6 s). `TestHelmCache…` and `TestTicksFetchHelmReleasesOnlyWhenTheyChange` |
| The same 1,000 GETs on the first tick after a start, and on every one-shot `scan` | Sequential, 36 s and 24 CPU-seconds here; reached the step deadline at the old 200m default (measured, [above](#cpu-and-the-chart-limit)) or over a slow link; a concurrent fetch spends the same CPU-seconds | Open (the chart's default limit is now 1 CPU, so it no longer gives up on 1,000 releases): bounded-concurrency fetching ([#226](https://github.com/abd-ulbasit/upgradescope/issues/226)) |
| `kube-system` pods are listed in full twice a tick: by the control-plane version check, then again by the all-pods list | 4,009 pods here; the second listing is not a separate 9 requests but 9 extra ones (those pods already fall inside the 29 pages of the all-pods list) and about 4,000 pods decoded twice; grows with nodes (every node adds a `kube-proxy` and a CNI pod) | **Fixed** by #232 (each pod is read once): 30 pod list requests a tick instead of 38 in the runs on `735751d` ([#227](https://github.com/abd-ulbasit/upgradescope/issues/227)) |
| Argo CD Applications and Flux HelmReleases are listed whole (page size 50) every tick, plus a GET per distinct OCIRepository (#218) | **Measured** (`BENCH_GITOPS=1`): 540 of the 596 requests of a steady tick at 1,000 Applications and 1,000 HelmReleases, 500 of them sequential OCIRepository GETs; 12 MiB of 63; 8.7 more seconds and 3.2 more CPU-seconds, under host noise ([above](#re-measured-on-main-with-and-without-argo-cd-and-flux)) | Open: the OCIRepository GETs could be a list per namespace; not filed |
| The all-pods list is the steady tick's largest cost | 30 of 53 requests (38 of 61 before #232); whole pod objects are needed for their images, and the agent keeps no watch | Open: needs a decision on frequency ([#228](https://github.com/abd-ulbasit/upgradescope/issues/228)) |

## The server

`hack/bench/serve.sh` starts `serve` in-process (default target plus two
`--targets`, so three evaluations per snapshot), and 200 clusters push
through the real HTTP handler as the agent does: gzip body, bearer token,
the agent's retry rules. Three rounds per concurrency level: `new` (every
cluster's first snapshot), `changed` (a new inventory from each) and
`duplicate` (the hourly force-sync of an unchanged inventory, which stores
nothing). With 1 pusher, 16, and 200 (every cluster at the same moment).

Throughput is pushes a second, latency is what the pusher sees from sending
to the answer, database growth is the file (SQLite, with its WAL) or
`pg_database_size` (Postgres) over the new snapshots. The SQLite figure counts
the WAL and page allocation along with the data, so it reads high on a small
fleet (a 20-cluster trial read 135 to 210 KiB a snapshot): treat the
extrapolations below as upper estimates.

| Backend | Pushers | Round | Pushes/s | p50 ms | p99 ms | CPU ms/push | Peak heap MiB | DB KiB/snapshot |
|---|---|---|---|---|---|---|---|---|
| SQLite | 1 | new | 51.8 | 9.8 | 101.6 | 26.1 | 9 | 106.1 |
| SQLite | 1 | changed | 49.6 | 8.4 | 109.1 | 26.7 | 10 | 99.1 |
| SQLite | 1 | duplicate | 162.5 | 4.5 | 27.3 | 7.2 | 7 | 0 |
| SQLite | 16 | new | 53.6 | 306.6 | 377.2 | 25.8 | 10 | 106.1 |
| SQLite | 16 | changed | 52.6 | 312.9 | 379.7 | 26.2 | 10 | 99.4 |
| SQLite | 16 | duplicate | 176.2 | 89.2 | 114.6 | 7.1 | 9 | 0 |
| SQLite | 200 | new | 56.0 | 1,623 | 3,526 | 24.0 | 32 | 105.3 |
| SQLite | 200 | changed | 55.0 | 1,731 | 3,619 | 24.6 | 34 | 96.0 |
| SQLite | 200 | duplicate | 171.9 | 582 | 1,127 | 7.4 | 27 | 0 |
| Postgres | 1 | new | 38.6 | 18.8 | 120.5 | 27.8 | 10 | 19.6 |
| Postgres | 1 | changed | 38.3 | 16.6 | 118.5 | 28.1 | 10 | 18.9 |
| Postgres | 1 | duplicate | 111.7 | 7.3 | 32.0 | 7.8 | 7 | 0 |
| Postgres | 16 | new | 40.6 | 402.9 | 478.2 | 27.1 | 11 | 19.6 |
| Postgres | 16 | changed | 39.8 | 408.2 | 504.1 | 27.6 | 11 | 18.9 |
| Postgres | 16 | duplicate | 118.6 | 133.1 | 163.7 | 7.5 | 10 | 0 |
| Postgres | 200 | new | 40.4 | 2,293 | 4,816 | 26.2 | 33 | 19.4 |
| Postgres | 200 | changed | 41.0 | 2,180 | 4,756 | 26.3 | 33 | 18.8 |
| Postgres | 200 | duplicate | 113.2 | 884 | 1,722 | 8.1 | 29 | 0 |

No push failed or was retried in any round; the harness output also gives
the maximum latency and the attempts.

- **The server is CPU-bound, not database-bound**: about 25 CPU-ms per new
  snapshot whatever the database (the whole process, load generator
  included: decoding, validating, storing and evaluating three targets) and 7
  for an unchanged one. Throughput stops growing past one pusher because the
  two cores do. Latency at 200 pushers is queueing: the last cluster waits
  for the 199 before it. The chart's server limit of 500m would still allow
  about 20 snapshots a second at 25 CPU-ms each (computed, not measured).
- **A fleet is far below that.** 200 clusters push at most once per tick
  (0.33 a second at the 10-minute default); even if every inventory changed
  every tick, that is under 1% of the 56 a second measured. The bottleneck of
  a fleet is the dashboard's reads, not ingest: see
  [Running the server](../operations.md#cpu-and-fleet-read-latency).
- **Postgres grows 5 times more slowly**: 19 KiB a snapshot against about
  100 KiB on SQLite for the same snapshots and their three evaluations each.
  Both keep the inventory and the reports as binary blobs; SQLite stores
  them as they are and Postgres compresses values over 2 KiB. Plan storage
  with [Retention and backup](retention-and-backup.md#retention-and-sizing):
  200 clusters whose inventory changes four times a day, kept 90 days, are
  72,000 snapshots, at most about 7 GiB on SQLite (an upper estimate) and 1.3 GiB on Postgres at
  these inventory sizes.
- **Postgres took about 30% less throughput here** (40 against 56 pushes a
  second) with the database a tenth of a millisecond away and sharing the
  same two cores. A database across a network adds its round trip to every
  statement.

## Reproducing

Both benchmarks are manual: they take minutes, fill a cluster, or start a
database, and never run in CI.

```sh
# Agent: needs a DISPOSABLE cluster. The kubeconfig is the only one read; the
# default kubeconfig and the current context are never used, and the script
# refuses a cluster with more than 3 real nodes or a namespace beyond kind's.
# BENCH_RUN_ON is an ssh host to run the measured ticks on (the cluster's
# host); without it they run here, so every request pays your network.
BENCH_RUN_ON=lab-host make bench-agent KUBECONFIG=/path/to/lab-kubeconfig

# The same with Argo CD Applications and Flux HelmReleases (the upstream CRDs,
# pinned by sha256, are installed into the lab; 1,000 of each at full size,
# BENCH_GITOPS_APPS and BENCH_GITOPS_HELMRELEASES change that).
BENCH_GITOPS=1 BENCH_RUN_ON=lab-host make bench-agent KUBECONFIG=/path/to/lab-kubeconfig

# The agent as a pod under the chart's CPU limit: fill the lab (BENCH_STEPS=1),
# build the image, kind load it, helm install the chart with
# agent.resources.limits.cpu set, then sample the pod's cgroup while it ticks
# (CFS periods and throttled periods, CPU used, peak memory and RSS).
KUBECONFIG=/path/to/lab-kubeconfig NODE_CONTAINER=<kind node container> \
  hack/bench/pod-sample.sh upgradescope <agent deployment> 1500 > samples.tsv

# Server: SQLite, then Postgres in a throwaway container on the engine
# `docker` is bound to (BENCH_RUN_ON=lab-host runs the server beside it).
BENCH_RUN_ON=lab-host make bench-ingest
BENCH_BACKENDS=sqlite make bench-ingest   # no Docker needed
```

`hack/bench/agent.sh` documents its knobs (`BENCH_STEPS`, `BENCH_TICKS`,
`BENCH_HELM_REVISIONS`, `BENCH_RESET_CMD`, `BENCH_NO_HELM_CACHE`, `BENCH_GITOPS`); both scripts print their tables
as above and keep the raw per-tick and per-round JSON lines in `bin/bench/`.
It needs `go`, `kubectl`, `jq` and `curl`. The KWOK controller is installed
into `kube-system` of the lab only, from release assets whose sha256 are
pinned in the script.

## What these numbers do not say

- They are one run on one machine: the wall times move with the load of the
  host (the apiserver shared two cores with KWOK, the benchmark and the two
  other clusters), the request and byte counts do not.
- A real apiserver of a cluster this size is bigger and busier than a kind
  control plane, with real kubelets, controllers and other clients. The
  counts apply; the latencies will differ either way.
- The agent as a pod ran under the chart's memory limit and three CPU
  limits, each for a first tick and one or two steady ones: a handful of
  ticks per limit, on a ThinkPad whose load average was 9 or more and whose
  lab control plane restarted once during the 1 CPU run. The throttling
  counts and CPU-seconds are the kernel's; the wall times vary with the
  host by a factor of two or more. The pod runs had no Argo CD or Flux
  objects, so what their reads add to the CPU limit (3.2 CPU-seconds a tick)
  is the benchmark's, not the pod's.
- The server benchmark generates inventories: real ones differ in the size
  of their Helm releases and the findings they carry. The per-snapshot
  storage scales with the inventory.
