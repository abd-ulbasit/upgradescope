# Scale and cost

What the agent costs a Kubernetes API server on a large cluster, and what
one `serve` can take from a fleet. The agent's current numbers, and the
before and after of the first tick (#226) and of the steady tick (#228),
come from runs on 4 October 2026 between 03:24 and 03:51 UTC and between
06:01 and 06:09 UTC, at the commits
[The tick after #226 and #228](#the-tick-after-226-and-228) names.
The older tables (#71's fix of the Helm releases, and the server) come from
the final runs of an earlier session the same day, between 04:30 and 05:00
in UTC+5 (the agent "before" table at 04:32, the agent "after" table at
04:45, the server table at 04:58; the raw result files are stamped in UTC,
which is the evening of 3 October). The harness is in `hack/bench/`; you can
run it against your own lab to get your own numbers. Read
[What is simulated](#what-is-simulated) before you quote them.

## The short answers

- **A steady agent tick on a cluster of 2,001 nodes, about 14,000 pods and
  1,000 Helm releases makes 24 API requests** (53 before
  [#228](https://github.com/abd-ulbasit/upgradescope/issues/228)) and reads
  50.3 MiB of responses (3.3 MiB on the wire, compressed) in 4.4 s and 3.1
  CPU-seconds (4.0 s and 3.0 before): the same bytes and CPU, because the
  requests are fewer but the pods to decode are not. A tick that asks API
  discovery again (at least hourly) makes 4 more. At the default interval
  of 10 minutes that is 0.04 requests a second.
- **The first tick after the agent starts costs more**: it reads each Helm
  release once, 1,030 requests in all. Since
  [#226](https://github.com/abd-ulbasit/upgradescope/issues/226) it fetches
  8 at a time: 27.1 s and 22.8 CPU-seconds beside the apiserver (36.1 s and
  23.1 before), and with 60 ms added to every round trip 29.6 s, where
  main's first tick reached the Helm step's deadline after 67.6 s with 231
  of the 1,000 releases unread. Before
  [#71](https://github.com/abd-ulbasit/upgradescope/issues/71) every tick
  cost that, because the agent fetched every release again each time.
- **Memory fits the chart's limit** (256Mi) with room: the peak RSS was 85.2
  MiB at full size, the first tick included, and the peak heap 58.5 MiB; the
  larger pod pages that cut the requests cost that (57.1 MiB of RSS and
  29.3 MiB of heap before). It is over the chart's memory request (64Mi), which
  is what the scheduler reserves, not a limit. CPU is what a large cluster
  uses: see [CPU](#cpu-and-the-chart-limit).
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
| upgradescope, #226 and #228 | the "before" runs: main `735751d` plus the recorder's per-resource bytes and time (`70d16ab`, which changes only the benchmark); #226 alone: `06cdf7a`; #226 and #228: `5da764e`, and `59d8561` (the final code: pages sized at 8 MiB rather than 16). Each run filled a freshly reset lab, or measured the fill a run before it left, with `BENCH_STEPS=1` (the full fill only) and the ticks run on the ThinkPad (`BENCH_RUN_ON`) |
| upgradescope, the older tables | the agent runs: main `a3e72ea` (#218, GitOps charts) plus this work; the "before" table is the same tree with the Helm step given no cache, which is what #71 changed. The server runs: main `f195ba5` plus this work (the server code is unchanged by this work; #217 is examples and tooling). The branch was rebased between the two, so its commit hashes name neither tree exactly. The numbers include #218's per-tick discovery and workload requests, but **its Argo CD and Flux lists are not measured**: the lab has neither tool's CRDs, so those lists are never made (see [Hotspots](#hotspots)) |
| Also running | for the older tables, two other idle kind clusters on the same ThinkPad; for #226 and #228, the same two (`bookstore`, and the other agent's lab `us-lab-137b`, which held no fill: 11 to 22% of a core and 530 to 840 MiB, `docker stats` before and after every run), at a host load average of 7 to 12 (4 threads), most of it lab A's KWOK controller (110 to 335% of a core at full size). For every run the lab's own KWOK controller kept 2,000 nodes alive |

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

### The tick after #226 and #228

Each run here is `BENCH_STEPS=1 BENCH_TICKS=5` (3 at the 60 ms round trip)
at the full fill, on lab A, on 4 October 2026 between 03:24 and 03:51 UTC
(and the `59d8561` row between 06:01 and 06:09 UTC), under the measurement
lock (lab B was up and held no fill, see
[What was measured](#what-was-measured-and-on-what)). "Before" is main
`735751d` with the recorder's per-resource counts (`70d16ab`), "#226" is
`06cdf7a`, "#226 + #228" is `5da764e`, whose pages were sized at 16 MiB rather
than the 8 MiB of `59d8561`, the final code: at 8 MiB the first page of
`kube-system`'s pods, whose average is larger than the other namespaces',
sized the next ones under 2,000, one pod request more. The 60 ms rows were measured with
`tc qdisc add dev eth0 root netem delay 60ms` in lab A's control-plane
container, which delays every packet it sends to the agent (so every
response) and nothing of lab B's; it was removed after the last of them.
A `5da764e` run beside the apiserver did not run (the download of the
pinned KWOK manifests failed for that checkout). A first attempt at the
`59d8561` rows (05:46 to 05:54 UTC) failed while seeding: the host's load
average reached 53 and lab B used 1.3 to 1.8 cores meanwhile, the seeder
lost its connection, and nothing of it is used. The rerun at 06:01 had lab B
idle again (12 to 21% of a core). Requests, bytes and CPU do not depend on
the round trip (compare the before and #226 rows at both).

| Tree | Round trip | Steady: requests | LIST pods | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB | First tick: requests, wall s, CPU s, Helm releases read |
|---|---|---|---|---|---|---|---|---|---|---|
| before | beside it | 53 | 30 | 50.1 | 3.4 | 4.0 | 3.0 | 29.3 | 57.1 | 1,053, 36.1, 23.1, 1,000 |
| #226 | beside it | 53 | 30 | 50.3 | 3.4 | 6.4 | 3.1 | 27.0 | 54.3 | 1,053, 30.0, 22.3, 1,000 |
| before | +60 ms | 53 | 30 | 50.7 | 3.4 | 7.4 | 3.0 | 26.7 | 52.7 | 821, 67.6, 10.6, **769** (the Helm step's deadline) |
| #226 | +60 ms | 53 | 30 | 50.7 | 3.4 | 8.3 | 3.0 | 29.3 | 53.6 | 1,053, 30.4, 22.2, 1,000 |
| #226 + #228 (`5da764e`) | +60 ms | 23 | 9 | 50.6 | 3.3 | 5.7 | 3.0 | 68.5 | 95.2 | 1,027, 29.6, 22.8, 1,000 |
| #226 + #228 (`59d8561`) | beside it | 24 | 10 | 50.3 | 3.3 | 4.4 | 3.1 | 58.0 | 85.2 | 1,030, 27.1, 22.8, 1,000 |

The steady columns are the median of the ticks after the first, as above
(the mean of the middle two of four; of two at 60 ms; the "before +60 ms"
row has one, its third tick, because its second read the 231 releases the
first had left). The wall times of steady ticks move with the host's load
(load average 7 to 16 on 4 threads, most of it lab A's KWOK at full size):
the #226 ticks beside the apiserver took 4.0 to 9.7 s for the same 53
requests and 3.0 CPU-seconds, which #226 does not change. The peak heap is
the heap objects the benchmark samples, garbage not yet collected included.

**The first tick (#226).** Where its time went before, by the recorder:
1,000 GETs of Helm Secrets spent 13.4 s waiting on the apiserver in all
(one at a time, so 13.4 s of the tick's 36.1), and the tick used 23.1
CPU-seconds, nearly all of it decoding the releases (a steady tick, which
decodes none, uses 3.0). Decoding dominates beside the apiserver; over a
slow link the wait does, a round trip per release. #226 fetches 8 at a
time, keeps decoding one at a time, and overlaps the two: 30.0 s beside the
apiserver, and at a 60 ms round trip 30.4 s, all 1,000 releases read, where
the tree before read 769 in the 67.6 s the Helm step's share of the tick
allows (the step's reason named the 231 unread, and the next tick read them
in another 35.0 s). So at 60 ms one first tick now does what took two, and
it is 2.2 times faster than the first tick before even though that one
stopped short (67.6 s against 30.4 s).

What bounds it, and why 8: `TestCollectHelmColdFetchAtRoundTrip` measures a
cold Helm step alone against a fake apiserver that answers every request
after 60 ms (1,000 generated releases in the lab's mix of sizes, 17.8 KiB
stored each on average; the clients `scan` and the agent build; on the Apple
M1 Pro at a load average of about 6, `5da764e`, 4 October 2026):

| Workers | Wall s | CPU s | Speedup |
|---|---|---|---|
| 1 | 65.04 | 4.16 | 1.00 |
| 2 | 32.58 | 3.74 | 2.00 |
| 4 | 16.46 | 3.46 | 3.95 |
| 8 | 14.27 | 3.53 | 4.56 |
| 16 | 14.27 | 3.40 | 4.56 |

Decoding those releases alone took 1.63 s there. Past 4 workers the step
reaches the client's own rate limit, 50 requests a second after a burst of
300 (`clientQPS`, `clientBurst`), which allows 1,000 GETs in no less than
(1,000 - 300) / 50 = 14 s (computed), so 16 workers gain nothing. 8 reaches
that floor with room for a slower link (at 120 ms, 8 workers wait 15 s,
computed) and holds at most 8 payloads. So the first tick's time is the
largest of three bounds: decoding (22 CPU-seconds here, one core, and more
under the chart's 200m limit, see [CPU](#cpu-and-the-chart-limit)), the rate
limit (14 s for 1,000 releases), and the round trip times the releases over
8.

**The steady tick (#228).** The recorder's split of the 53 requests and
50.1 MiB before, at full size:

| Verb | Resource | Requests | Response MiB | Time s (summed) |
|---|---|---|---|---|
| LIST | pods | 30 | 39.78 | 1.00 |
| LIST | nodes | 5 | 5.32 | 0.12 |
| GET | API discovery | 4 | 0.06 | 0.01 |
| LIST | secrets (metadata) | 3 | 0.42 | 0.02 |
| GET | `clusterreadinesses` | 2 | 0.02 | 0.02 |
| CREATE | `clusterreadinesses` (answered 409) | 1 | 0 | 0.02 |
| GET | `/metrics` | 1 | 4.38 | 0.28 |
| GET | `/version`, `kube-system` namespace | 1 each | 0 | 0.01, 0.02 |
| LIST | configmaps (metadata), CRDs, IngressClasses, namespaces | 1 each | 0, 0.08, 0, 0.04 | 0.01 or less each |
| UPDATE | `clusterreadinesses/status` | 1 | 0.01 | 0.02 |

The pods are 30 of the 53 requests and 79% of the bytes; nodes, discovery
and the agent's own object are most of the rest of the requests. #228 cut
them without a watch (AG-01) and without reading pods less often:

- The pod and node lists size their pages: 500, then as many as fit 8 MiB
  encoded at the size of the page before, at most 2,000 (the lab's pods and
  nodes are about 3 KiB, so up to 2,000). Pods: 30 requests to 10; nodes: 5
  to 2. The bound restated: a
  page after the first holds at most 8 MiB of objects as the page before
  measured them, which `TestPodPagePeakHeapIsBounded` reads as 34 MiB of
  live heap for production-sized pods (8 KiB in protobuf, managedFields
  included; about 1,000 a page) and 64 MiB for 40 KiB pods, which stay at
  500 a page as before. Here the larger pages raised the sampled heap peak
  from 29.3 to 58.0 MiB and the RSS from 57.1 to 85.2 MiB (to 68.5 and 95.2
  at 16 MiB).
- API discovery is kept between ticks (`collect.DiscoveryCache`, PF-14):
  4 requests to 0 on a steady tick. It is asked again (4 requests, so 28
  on that tick) when the
  server version changed (seen on the tick, before any step reads
  discovery), on the tick after the CRDs' groups, kinds or served versions
  changed or could not be listed, when it is an hour old, or when the last
  answer had an error. So discovery is at most one tick behind a CRD change
  and an hour behind any other change of the APIs a version serves.
- The agent reads its `ClusterReadiness` once and writes the status over
  it: 4 requests to 2 (one GET, one UPDATE; a CREATE only when it is
  missing, a second GET only on a conflict).
- Protobuf was already what the typed client asks for (#71). A list with
  `resourceVersion=0` is not used: it may be stale, which add-on detection
  would tolerate, but an apiserver whose watch cache cannot page it answers
  it whole, ignoring the limit, and the page is the bound.

The other requests are unchanged: `/metrics`, `/version`, the `kube-system`
namespace, and the metadata lists of the Helm Secrets (3), ConfigMaps,
CRDs, IngressClasses and namespaces. The bytes and the CPU are what they
were (50.3 MiB, 3.1 CPU-seconds): every pod is still decoded every tick,
which a pass every Nth tick (#228's option b) would change, and which 24
requests did not need. The split after, at `59d8561`:

| Verb | Resource | Requests | Response MiB | Time s (summed) |
|---|---|---|---|---|
| LIST | pods | 10 | 39.91 | 2.42 |
| LIST | secrets (metadata) | 3 | 0.43 | 0.04 |
| LIST | nodes | 2 | 5.32 | 0.14 |
| GET | `/metrics` | 1 | 4.52 | 0.24 |
| GET | `/version`, `clusterreadinesses`, `kube-system` namespace | 1 each | 0, 0.01, 0 | 0.01 or less each |
| LIST | configmaps (metadata), CRDs, IngressClasses, namespaces | 1 each | 0, 0.08, 0, 0.04 | 0.02 or less each |
| UPDATE | `clusterreadinesses/status` | 1 | 0.01 | 0.02 |

### The older tables (#71)

These are from the earlier session, at `a3e72ea` plus #71, before #232 (the
`kube-system` pods listed once), #226 and #228.

**Steady tick, after #71's fix** (Helm releases fetched only when new or
changed):

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

**The requests of a steady tick at full size then**, by verb and resource:

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

Everything is linear; nothing was superlinear. Most lists are paged at 500
objects, so a tick makes ceil(N / 500) requests per resource; the pod and
node lists, after a first page of 500, take as many objects a page as fit 8
MiB at the size of the page before, at most 2,000 (#228). At full size
after #226 and #228 that is 9 pod requests (`kube-system`'s 4,009 pods for
the version check, then the other namespaces' 10,000), 2 for nodes, 3 for
the metadata list of Helm Secrets, and 9 constant: `/version`, the
`kube-system` namespace, `/metrics`, the metadata lists of Helm ConfigMaps,
CRDs, IngressClasses and namespaces, and one GET and one status UPDATE of
the agent's `ClusterReadiness`. A tick that asks API discovery again (the
first, and the staleness rules above) adds 4, and a first tick one GET per
Helm release. A cluster with no Helm release (the empty row below: 22
requests before #228) also lists Deployments, StatefulSets and DaemonSets,
metadata only, to look for the tracking labels of Argo CD and Flux.
[Architecture](../architecture.md#api-cost-per-tick) states the formula.

The older tables say what the same fill cost before #232, #226 and #228:
61 requests, 38 of them pods, because the `kube-system` pods were listed
twice (#227).

### CPU and the chart limit

The tick is CPU-bound in the agent (the process also runs the benchmark's
byte counter, a small share): 3.0 CPU-seconds in 4.0 to 6 s of wall time
when steady, 22.8 in 27.1 s when it reads 1,000 releases (after #226; 23.1
in 36.1 s before; the older tables' 3.6 and 24.4 include the `kube-system`
pods decoded twice). The chart's default limit
for the agent is 200m (`agent.resources.limits.cpu`), which allows 0.2
CPU-seconds a second, so those ticks last at least 15.5 s and 114 s there
(computed from the CPU time, not measured under a cgroup quota; measuring it
is [#229](https://github.com/abd-ulbasit/upgradescope/issues/229)). The tick
deadline is half the interval, 5 minutes at the default, and the Helm step,
the second of six, gets a fifth of the time left, about a minute, so a first
tick of 1,000 releases at 200m will probably reach the step's deadline and
leave some releases unread (a gap the report names); the next tick reads the
rest, because the cache keeps what was decoded. #226's concurrent fetch
does not help there: under a CPU quota the decoding is the bound, and it
stays one release at a time. On a cluster this size, give
the agent 500m to 1 CPU, or expect its first ticks to be partial.

### Hotspots

| What | Found | Status |
|---|---|---|
| One GET per Helm release on every tick: 1,000 of 1,061 requests, 90 MiB, 33 s and 23.5 CPU-seconds at full size | The only cost that grew with releases, not pages | **Fixed**: the agent keeps what it decoded, keyed by the storage object's UID and resourceVersion; a steady tick makes none (61 requests then, 23 after #228). `TestHelmCache…` and `TestTicksFetchHelmReleasesOnlyWhenTheyChange` |
| The same 1,000 GETs on the first tick after a start, and on every one-shot `scan` | Sequential, 36.1 s and 23.1 CPU-seconds here; at a 60 ms round trip the first tick reached the Helm step's deadline with 231 releases unread | **Fixed** ([#226](https://github.com/abd-ulbasit/upgradescope/issues/226)): 8 GETs in flight, decoded one at a time in order; 27.1 s here, 29.6 s at 60 ms with every release read. Decoding (22 CPU-seconds) and the client's rate limit (14 s for 1,000) are the bounds now. `TestCollectHelmConcurrentMatchesSequential` |
| `kube-system` pods are listed in full twice a tick: by the control-plane version check, then again by the all-pods list | 4,009 pods here; the second listing is not a separate 9 requests but 9 extra ones (those pods already fall inside the 29 pages of the all-pods list) and about 4,000 pods decoded twice; grows with nodes (every node adds a `kube-proxy` and a CNI pod) | **Fixed** ([#227](https://github.com/abd-ulbasit/upgradescope/issues/227), #232): the add-ons take them from the version check's list |
| Argo CD Applications and Flux HelmReleases are listed whole (page size 50) every tick, plus a GET per distinct OCIRepository (#218) | **Not measured**: the lab has neither tool installed, so the harness never makes these requests. ceil(N / 50) per tool grows faster than a metadata list (whole objects, small pages) | Open, #233 |
| The all-pods list is the steady tick's largest cost | 38 of 61 requests (30 of 53 after #232), 79% of the bytes; whole pod objects are needed for their images, and the agent keeps no watch | **Requests fixed** ([#228](https://github.com/abd-ulbasit/upgradescope/issues/228)): pod and node pages sized by bytes, discovery kept between ticks, the agent's object read once: 53 to 24 requests. The bytes (50 MiB) and CPU (3 s) are unchanged, since every pod is still decoded every tick; reading pods less often (#228's option b) would cut those, and was not needed for the request count |

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

A cold Helm step against a fake apiserver a round trip away, for 1, 2, 4,
8 and 16 workers (#226), needs no cluster:

```sh
UPGRADESCOPE_BENCH_HELM_RTT=60ms go test -run TestCollectHelmColdFetchAtRoundTrip -v ./internal/collect
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
- The 60 ms round trip is a delay netem adds to what lab A's control plane
  sends: latency only, no bandwidth limit and no loss. The fake apiserver of
  `TestCollectHelmColdFetchAtRoundTrip` adds it as a sleep before each
  answer, and serves generated releases, so the decoding there is a laptop's
  and not the lab's.
