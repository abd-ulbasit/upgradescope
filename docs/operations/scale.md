# Scale and cost

What the agent costs a Kubernetes API server on a large cluster, and what
one `serve` can take from a fleet. Three sets of runs on 4 October 2026
give the agent's numbers, each table naming its own:

- **The code of #226 and #228** (the concurrent fetch of a cold Helm step,
  and the cheaper steady tick), measured before and after between 03:24
  and 03:51 UTC, between 06:01 and 06:09 UTC, and between 06:55 and 07:03
  UTC, at the commits [The tick after #226 and #228](#the-tick-after-226-and-228)
  names. These ran in the benchmark process, outside a CPU quota, and
  without Argo CD or Flux objects.
- **Main `735751d`**, before #226 and #228 (times in UTC: 03:02 to 05:42):
  the tick re-measured without and with Argo CD and Flux objects
  ([Re-measured on main](#re-measured-on-main-with-and-without-argo-cd-and-flux)),
  and the agent run as a pod under three CPU limits ([CPU and the chart
  limit](#cpu-and-the-chart-limit)). The code of #226 and #228 has not been
  run with the GitOps fill or as a pod; that is to be measured when a
  release is cut.
- **The older tables** (#71's fix of the Helm releases, and the server) are
  from the final runs of an earlier session the same day, between 04:30
  and 05:00 in UTC+5 (the agent "before" table at 04:32, the agent "after"
  table at 04:45, the server table at 04:58; the raw result files are
  stamped in UTC, which is the evening of 3 October).

The harness is in `hack/bench/`; you can run it against your own lab to get
your own numbers. Read [What is simulated](#what-is-simulated) before you
quote them.

## The short answers

- **A steady agent tick on a cluster of 2,001 nodes, about 14,000 pods and
  1,000 Helm releases makes 31 API requests** at the code of
  [#228](https://github.com/abd-ulbasit/upgradescope/issues/228) (`9810fb6`,
  4 October 2026, 06:55 to 07:03 UTC), whose target of under 25 this does
  not meet, and reads 50.4 MiB of responses (3.1 MiB on the wire,
  compressed) in 3.7 s and 3.0 CPU-seconds. On main `735751d`, before
  #228, it made 53 requests and read 50 MiB (3.4 MiB on the wire) in 4.1 s
  and 3.0 CPU-seconds, with a peak live heap of 28 MiB and a peak RSS of
  56 MiB (the earlier session's 61 requests, 5.6 s and 3.6 CPU-seconds on
  `a3e72ea` are in [the older tables](#the-older-tables-71), and the 8
  requests fewer are #232 reading each `kube-system` pod once). #228 cut
  the requests, not the bytes or the CPU: the pods to decode are the same.
  An earlier draft made 24, with pod pages of up to 2,000, whose worst case
  was past the agent's memory limit (see [The steady
  tick](#the-tick-after-226-and-228)). A tick that asks API discovery again
  (at least hourly) makes 4 more. At the default interval of 10 minutes,
  31 requests are 0.052 requests a second (53 were 0.088).
- **The first tick after the agent starts costs more**: it reads each Helm
  release once. On main `735751d` that was 1,053 requests in all, 36.7 s and
  23.5 CPU-seconds. Since
  [#226](https://github.com/abd-ulbasit/upgradescope/issues/226) the agent
  fetches 8 at a time: 1,037 requests, 42.4 s and 22.0 CPU-seconds beside
  the apiserver at `9810fb6`, on a host whose load average rose from 1.9 to
  15.4 while it ran, where the 1,000 GETs waited 99.0 s in all (36.1 s and
  23.1 in the #226/#228 runs of `735751d` (with the recorder, `70d16ab`), at
  a load of 7 to 12; 27.1 s and 22.8 at the draft `59d8561`, at that load
  too, where the GETs waited 48.5 s). With 60 ms added to every round trip
  it took 30.4 s at `06cdf7a` (#226 alone) and 29.6 s at the draft
  `5da764e`, where main's first tick reached the Helm step's deadline after
  67.6 s with 231 of the 1,000 releases unread. Before
  [#71](https://github.com/abd-ulbasit/upgradescope/issues/71) every tick
  cost that, because the agent fetched every release again each time: it was
  the one cost that grew with releases rather than with pages (1,000 GETs of
  1,061 requests).
- **Argo CD and Flux add a request per fifty objects**: with 1,000
  Applications and 1,000 HelmReleases (half of them with a `chartRef` to an
  OCIRepository of their own) a steady tick on main `735751d` made 596
  requests, 543 more, 500 of them one GET per OCIRepository, and read
  63 MiB, 12.8 s and 6.2 CPU-seconds. Since
  [#248](https://github.com/abd-ulbasit/upgradescope/issues/248) the
  OCIRepositories are listed, paged at 50 like the other lists: at `da90a86e`
  (with #228) the same fill's steady tick made 81 requests, 50 of them
  GitOps (540 before), read 59.8 MiB, and took 7.5 s and 5.3 CPU-seconds on a
  host at a load average of 20 to 28. See
  [the tables](#re-measured-on-main-with-and-without-argo-cd-and-flux) and
  [after #248](#the-ocirepositories-listed-248).
- **Memory fits the chart's defaults** (64Mi request, 256Mi limit) with room
  at this fill: on main `735751d` the peak RSS of the benchmark never passed
  61 MiB, and the agent as a pod reached 71 MiB (`VmHWM`), the first tick
  included. At #228's code the benchmark's peak RSS was 65.0 MiB and its
  peak heap 39.2 MiB (57.1 and 29.3 MiB in the #226/#228 runs of `735751d`
  (with the recorder, `70d16ab`)): the larger pod pages that cut the
  requests cost that; no pod was run at that code. That is above the 64Mi
  request, which only informs scheduling. A pod page is bounded by its count
  only, at most 1,000 pods, so its worst case is 1,000 times the largest
  pod, whatever its size, reached when small pods are followed by large
  ones. 1,000 pods of about 40 KiB after small ones measured 124.5 to 125.5
  MiB of live heap in a test (`TestPodPagePeakHeapIsBounded`, which allows
  128 MiB); at that rate, about 3.2 bytes of live heap per encoded byte,
  pods of about 70 KiB after small ones would take one page past the agent's
  `GOMEMLIMIT` (about 230 MiB) and likely past the limit, where the pages of
  500 before #228 would have reached it at about 145 KiB (computed, not
  measured; see [the steady tick](#the-tick-after-226-and-228)).
- **The chart's default CPU limit was too low for such a cluster, and is now
  1 CPU.** As a pod at the old 200m the first tick gave up at its Helm
  step's deadline with 132 of 1,001 releases unread, and a steady tick took
  21 s instead of 4 (main `735751d`). #226's concurrent fetch does not
  change that: it waits on the apiserver less, but decodes as before, and
  under a quota the decoding is the bound. See [CPU](#cpu-and-the-chart-limit).
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
| The later runs | main `735751d` for the agent, chart and collector (main has since moved to `a93ba31`, which changes `internal/engine` and `internal/suppress`, the evaluate step, and the server; not the collector, the agent or the chart's agent values: it edits a docs-link comment under `server.ingress` in `values.yaml`, and the chart README), with #249's harness (`hack/bench/`, the GitOps fill, `pod-sample.sh`). The pod runs used a `linux/amd64` binary cross-compiled on the Mac with the same flags as `Dockerfile` (`CGO_ENABLED=0`, `-trimpath`, `-s -w`), packed with `Dockerfile.release` by `docker build` on the ThinkPad's engine and loaded with `kind load docker-image`: the dashboard bundle it embeds is the committed one, not a rebuilt one. Lab `us-lab-137b` (kind 1.37.0, a second cluster beside the first on the same ThinkPad), Argo CD Application CRD v3.5.3, Flux helm-controller CRDs v1.6.5 and source-controller v1.9.6 (OCIRepository only), pinned by sha256 in `hack/bench/agent.sh` |
| upgradescope, #226 and #228 | the "before" runs: main `735751d` plus the recorder's per-resource bytes and time (`70d16ab`, which changes only the benchmark); #226 alone: `06cdf7a`; #226 and #228: `5da764e` and `59d8561` (drafts: pages sized from the average object, up to 2,000; at 16 MiB and at 8 MiB), and `9810fb6`, the final code (pages sized from the largest object, up to 1,000, after review). These are the branch's commits as measured, on `735751d`; rebased onto main `a0d9652` (#249) they are `3c1fe6f`, `ba33886`, `13901f4` and `419d624`, the same collector and agent with main's changes since `735751d` (#235's server and engine, #249's harness and the chart's 1 CPU default), which the runs did not have. Each run filled a freshly reset lab (lab A, the first of the two), or measured the fill a run before it left, with `BENCH_STEPS=1` (the full fill only) and the ticks run on the ThinkPad (`BENCH_RUN_ON`) |
| Also running | two other idle kind clusters on the same ThinkPad, and for the agent runs the lab's own KWOK controller keeping 2,000 nodes alive |
| Also running, later runs | the lab's KWOK controller, and a second lab that another session used on the same ThinkPad. Its control-plane container used 11 to 19% of one core and 565 to 945 MiB whenever it was sampled (`docker stats`, at each fill level and at the start and end of the pod runs): the idle figure of a kind control plane, not a 2,000-node fill, which takes 2.4 to 3.6 cores. The ThinkPad's load average was 2.4 at the start of the GitOps run, and 9 to 25 during its fills and the pod runs (4 threads), so the wall times of the later runs are noisier than the counts and CPU-seconds, and are given as ranges |
| Also running, #226 and #228 | the same two clusters (`bookstore`, and the other session's lab `us-lab-137b`, which held no fill then: 11 to 22% of a core and 530 to 840 MiB, `docker stats` before and after every run; 13 to 15% and 681 to 704 MiB around the `9810fb6` run), at a host load average of 7 to 12 (4 threads; 1.9 when the `9810fb6` run started and 15.4 when it ended), most of it lab A's KWOK controller (110 to 335% of a core at full size) |
| upgradescope, #248 | `da90a86e` (branch `fix/collect-helm-gitops` on main `345a879`, with #226 and #228): `BENCH_GITOPS=1 BENCH_STEPS=1`, the ticks run on the ThinkPad (`BENCH_RUN_ON`), 9 October 2026, 15:28 to 15:35 UTC, on lab `us-lab-137b` ([the run](#the-ocirepositories-listed-248)) |
| Also running, #248 | `bookstore` (12 to 22% of a core) and lab `us-lab-137`, another session's, up and not idle: 10 to 90% of a core and 654 MiB, so holding no full fill; host load average 12 to 28 (4 threads) |

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
agent builds, wrapped to count requests, response bytes and time by verb
and resource; a TCP forwarder counts the bytes on the wire. Each row is the
median of the ticks after the first (with the default 5 ticks, 4: the mean
of the two middle values; the peak heap is the maximum of them); the first,
which fills the Helm cache, is beside it.

Full size is 2,001 nodes (2,000 fake and the control plane), 10,000 seeded
pods plus the 4,000 DaemonSet pods, 6,000 ConfigMaps, 4,000 Deployments, 100
namespaces and 1,000 Helm release Secrets.

### The tick after #226 and #228

Each run here is `BENCH_STEPS=1 BENCH_TICKS=5` (3 at the 60 ms round trip)
at the full fill, on lab A, on 4 October 2026 between 03:24 and 03:51 UTC
(the `59d8561` row between 06:01 and 06:09 UTC, the `9810fb6` row between
06:55 and 07:03 UTC), under the measurement lock (lab B was up and held no
fill, see [What was measured](#what-was-measured-and-on-what)). The agent
ran in the benchmark process, outside a CPU quota, and the lab had no Argo
CD or Flux objects. "Before" is main `735751d` with the recorder's
per-resource counts (`70d16ab`), "#226" is `06cdf7a`. The "#226 + #228"
rows are three versions of the page sizing: `5da764e` and `59d8561`,
drafts that sized a page from the average object of the page before, up to
2,000, at 16 MiB and 8 MiB (at 8 MiB the first page of `kube-system`'s
pods, whose average is larger than the other namespaces', sized the next
ones under 2,000, one pod request more); and `9810fb6`, the page sizing as
shipped, which sizes it from the largest object, up to 1,000 (the commits
after it change only the docs and the words of the reason for a Helm
release left unread past the step's deadline, which none of these runs
reached), because review found the drafts' worst case past the memory
limit (see the steady tick, below). The 60 ms rows were measured with
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
| #226 + #228 (`9810fb6`, as shipped) | beside it | 31 | 16 | 50.4 | 3.1 | 3.7 | 3.0 | 39.2 | 65.0 | 1,037, 42.4, 22.0, 1,000 |

The steady columns are the median of the ticks after the first, as above
(the mean of the middle two of four; of two at 60 ms; the "before +60 ms"
row has one, its third tick, because its second read the 231 releases the
first had left). The wall times of steady ticks move with the host's load
(load average 7 to 16 on 4 threads, most of it lab A's KWOK at full size):
the #226 ticks beside the apiserver took 4.0 to 9.7 s for the same 53
requests and 3.0 CPU-seconds, which #226 does not change. So does the
first tick's: `9810fb6`'s took 42.4 s against `59d8561`'s 27.1 s, for 22.0
and 22.8 CPU-seconds, and its 1,000 Helm GETs waited 99.0 s in all against
48.5 s, while the host's load average rose from 1.9 to 15.4 (lab A's
control plane used 301% of a core at the end, lab B 13%); between the two
only the page sizing of the pod and node lists and what the Helm step does
past its deadline changed, and both read all 1,000 releases. The "before"
row is a run of the same tree as [Re-measured on
main](#re-measured-on-main-with-and-without-argo-cd-and-flux) below, under
another host load: 4.0 s and 36.1 s there against 4.1 s and 36.7 s, the
same 53 and 1,053 requests. The peak heap is the heap objects the
benchmark samples, garbage not yet collected included.

**The first tick (#226).** Where its time went before, by the recorder:
1,000 GETs of Helm Secrets spent 13.4 s waiting on the apiserver in all
(one at a time, so 13.4 s of the tick's 36.1), and the tick used 23.1
CPU-seconds, nearly all of it decoding the releases (a steady tick, which
decodes none, uses 3.0). Decoding dominates beside the apiserver; over a
slow link the wait does, a round trip per release. #226 fetches 8 at a
time, keeps decoding one at a time, and overlaps the two (PF-15): at `06cdf7a`,
30.0 s beside the apiserver, and at a 60 ms round trip 30.4 s, all 1,000
releases read, where the tree before (main `735751d`) read 769 in the
67.6 s the Helm step's share of the tick allows (the step's reason named
the 231 unread, and the next tick read them in another 35.0 s). So at
60 ms one first tick now does what took two, and it is 2.2 times faster
than the first tick before even though that one stopped short (67.6 s
against 30.4 s).

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
largest of three bounds: decoding (22 CPU-seconds here, one core, and
longer under a CPU quota, see [CPU](#cpu-and-the-chart-limit)), the rate
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
  encoded at the size of the largest object of the page before, at most
  1,000 (the lab's pods and nodes are about 3 KiB, so 1,000). Pods: 30
  requests to 16; nodes: 5 to 3. A page's limit is set before its objects
  are seen, so nothing bounds its bytes but the count: its worst case is
  1,000 times the largest object, reached when small objects are followed by
  large ones (pods are listed by namespace). `TestPodPagePeakHeapIsBounded`
  measures it: 500 pods of 137 bytes, then 1,000 of up to 41,685 bytes (39.4
  MiB encoded), peaked at 124.5 to 125.5 MiB of live heap, under half the
  chart's 256Mi but only 2.5 to 3.5 MiB under the test's own limit of 128
  MiB, so a change that makes a page's decoding about 2 to 3% larger fails
  it; a run of those pods, which stays at 500 a page as before, at 63.2 to
  63.7 MiB; production-sized pods (about 8 KiB in protobuf, managedFields
  included; about 990 a page) at 34.5 to 35.4 MiB (Apple M1 Pro, 4 October
  2026). The 40 KiB pods are an example, not the bound: at the rate they
  measured, about 3.2 bytes of live heap per encoded byte (125.5 MiB for
  39.4 MiB), one page of 1,000 pods passes the agent's `GOMEMLIMIT` (90% of
  256Mi, about 230 MiB) at pods of about 70 KiB after a page of small ones
  (230 MiB / 3.2 / 1,000 is 74 KiB, less what the rest of the agent holds),
  where pages of at most 500, before #228, would have reached it at about
  145 KiB (230 MiB / 3.2 / 500 is 147 KiB; computed, not measured). Pods
  that large exist: Argo Workflows pods carry their template, for example.
  The drafts sized a page from the average object of the page before, up to
  2,000: 10 pod and 2 node requests, 24 in all, but they ask for 2,000 of
  the large pods after the small ones, and the same test with pages of 500,
  2,000 and 500 peaked at 249.4 to 250.0 MiB of live heap, past the agent's
  `GOMEMLIMIT` (90% of 256Mi), and likely past the 256Mi limit (inferred
  from the live heap: no agent was run at that limit). At most 1,000 a page,
  no page holds more than twice the objects a page held before #228. Here
  the larger pages raised the sampled heap peak from 29.3 to 39.2 MiB and
  the RSS from 57.1 to 65.0 MiB (58.0 and 85.2 at `59d8561`, 68.5 and 95.2
  at `5da764e`).
- API discovery is kept between ticks (`collect.DiscoveryCache`, PF-16):
  4 requests to 0 on a steady tick. It is asked again (4 requests more on
  that tick) when the server version changed (seen on the tick, before any
  step reads discovery), on the tick after the CRDs' groups, kinds or
  served versions changed or the CRD list failed, when it is an hour old,
  or when the last answer had an error. Custom resources that could not be
  listed are not a reason: the agent is granted none, so a CRD with a
  deprecated or unserved version makes the `crds` capability partial on
  every tick, and until `fd0b073` that asked discovery again on every tick
  of such a cluster (the lab has no such CRD, so its numbers were not
  affected). So discovery is at most one tick behind a CRD change and an
  hour behind any other change of the APIs a version serves, with one
  exception: the CRDs it is compared with are read by the `crds` step,
  after `api-usage` asked for discovery, so a CRD changed between those two
  steps of the tick that asks is taken as what discovery saw, and is seen
  when the answer is dropped for another reason, at the latest an hour
  later. The GitOps detection's discovery, which asks which versions of
  the Argo CD and Flux groups are served (3 requests more with their CRDs
  installed, [below](#re-measured-on-main-with-and-without-argo-cd-and-flux)),
  is answered from the same cache (`TestDiscoveryCacheServesSteadyTicks`;
  not measured with the GitOps fill).
- The agent reads its `ClusterReadiness` once and writes the status over
  it (PF-17): 4 requests to 2 (one GET, one UPDATE; a CREATE only when it
  is missing, a second GET only on a conflict).
- Protobuf was already what the typed client asks for (#71). A list with
  `resourceVersion=0` is not used: it may be stale, which add-on detection
  would tolerate, but an apiserver whose watch cache cannot page it answers
  it whole, ignoring the limit, and the page is the bound.

The other requests are unchanged: `/metrics`, `/version`, the `kube-system`
namespace, and the metadata lists of the Helm Secrets (3), ConfigMaps,
CRDs, IngressClasses and namespaces. The bytes and the CPU are what they
were (50.4 MiB, 3.0 CPU-seconds): every pod is still decoded every tick.
**31 requests miss #228's target of under 25.** What would reach it is
not safe or not done: pages of up to 2,000 (24 requests) have the worst
case above; reading the pods less often (#228's option b) would cut the
pod requests and the bytes and CPU with them, but needs a staleness bound
and a setting of its own, and is not part of this work. The split after,
at `9810fb6`:

| Verb | Resource | Requests | Response MiB | Time s (summed) |
|---|---|---|---|---|
| LIST | pods | 16 | 40.10 | 1.09 |
| LIST | nodes | 3 | 5.32 | 0.12 |
| LIST | secrets (metadata) | 3 | 0.42 | 0.04 |
| GET | `/metrics` | 1 | 4.43 | 0.23 |
| GET | `/version`, `clusterreadinesses`, `kube-system` namespace | 1 each | 0, 0.01, 0 | 0.01 or less each |
| LIST | configmaps (metadata), CRDs, IngressClasses, namespaces | 1 each | 0, 0.08, 0, 0.04 | 0.02 or less each |
| UPDATE | `clusterreadinesses/status` | 1 | 0.01 | 0.04 |

### Re-measured on main, with and without Argo CD and Flux

Two runs of the same harness on 4 October 2026 (UTC), on main `735751d`,
before #226 and #228, the tick being the agent's own as in the tables
above. The first is the cluster at full size with no Argo CD or Flux CRD
installed (`BENCH_STEPS="0 1"`, 5 ticks each, stamped 03:53 UTC). The
second is `BENCH_GITOPS=1`: the three upstream CRDs installed in the lab,
and at each level Argo CD Applications and Flux HelmReleases in proportion
to the fill (1,000 of each at full size: half of the Applications with
`spec.sources`, half of the HelmReleases with a `chartRef` to an
OCIRepository of their own, 500 of those; every one deploys to the scanned
cluster and every chart resolves, so the collector reads 2,000 charts). The
levels 0, 1/4 and 1/2 of the second run are one invocation (03:03 UTC).
Seeding the full level in that invocation failed (`etcdserver: request
timed out`, the loaded lab's etcd), so the full level is a second
invocation of `BENCH_STEPS=1` that continued the fill (03:18 UTC) and
measured its 5 ticks; the seeder now retries such errors. The levels below
it were not measured again. The benchmark failed any run in which the
collector did not read back every seeded chart, and accepted one gap: the
helm capability is partial with only Argo CD skipped, which is the
documented gap for charts Argo CD renders without a release.

The agent, no GitOps objects (the CRDs absent):

| Fill | Nodes | Helm releases | Requests | Of them LIST pods | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB | First tick: requests, response MiB, wall s, CPU s |
|---|---|---|---|---|---|---|---|---|---|---|---|
| empty | 1 | 0 | 22 | 2 | 4.7 | 0.4 | 0.5 | 0.5 | 21.6 | 55.2 | 22, 4.6, 2.6, 0.5 |
| full | 2,001 | 1,000 | 53 | 30 | 50.4 | 3.4 | 4.1 | 3.0 | 28.5 | 55.8 | 1,053, 72.6, 36.7, 23.5 |

The four steady ticks at full size took 3.4 to 4.7 s and 2.8 to 3.2
CPU-seconds. The one request-count difference from the older tables is
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
  versions of the three groups are served, once each. Since #228 a steady
  tick asks none of the 7 (above; from the code and its test, not measured
  with the fill). #228 changes none of the 540 GitOps requests: their lists
  are paged at 50; the OCIRepository GETs were
  [#248](https://github.com/abd-ulbasit/upgradescope/issues/248), below.

### The OCIRepositories listed (#248)

Since #248 a HelmRelease's `chartRef` is resolved from a list of the
OCIRepositories, not a GET each: one list of the namespace the chartRefs
point into when they all point into one, else one cluster-wide list, both
paged at 50; per namespace when the cluster-wide list is forbidden; and a
GET by name only in a namespace whose list is forbidden too. An
OCIRepository that cannot be read is still an unresolved chartRef, a named
partial gap, as before. The chart's `rbac.gitops.flux` grants `list` on
OCIRepositories beside `get`.

One run of `BENCH_GITOPS=1 BENCH_STEPS=1` (the full fill only, 5 ticks) at
`da90a86e` (this branch, on main `345a879`, so with #226 and #228), 9
October 2026, 15:28 to 15:35 UTC, on lab `us-lab-137b`, which held no fill
before the run (its control plane at 39% of a core a minute before; a full
fill keeps it at 1 to 3 cores) and was filled by it: 2,001 nodes, 1,000 Helm releases, 1,000 Applications and
1,000 HelmReleases (500 chartRefs, each to an OCIRepository of its own in
the HelmRelease's namespace, the HelmReleases spread over the fill's 100
namespaces, so the collector made one cluster-wide list). The other lab, `us-lab-137`, was up and
not idle: its control plane used 10 to 90% of a core during the run (17
and 13% at the two samples around the measured ticks) and 654 MiB of
memory, against 3.3 GiB for this lab's full fill. The ThinkPad's load
average was 12 to 28 over the run and 20 to 28 around the measured ticks,
on 4 threads, so the wall times are a loaded host's. The benchmark read
back every seeded chart (2,000).

| Fill | Requests | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB | First tick: requests, response MiB, wall s, CPU s |
|---|---|---|---|---|---|---|---|---|
| full, GitOps (`da90a86e`) | 81 | 59.8 | 5.5 | 7.5 | 5.3 | 41.6 | 66 | 1,090, 82.1, 30.3, 25.2 |
| full, GitOps (`735751d`, above) | 596 | 63.2 | 5.4 | 12.8 | 6.2 | 31.6 | 60.8 | 1,596, 85.5, 44.4, 26.2 |

| Fill | Charts read | LIST Applications: requests, MiB | LIST HelmReleases: requests, MiB | OCIRepositories: requests, MiB | GitOps total: requests, MiB |
|---|---|---|---|---|---|
| full (`da90a86e`) | 2,000 | 20, 7.46 | 20, 1.18 | 10 LISTs, 0.39 | 50, 9.04 |
| full (`735751d`, above) | 2,000 | 20, 7.46 | 20, 3.65 | 500 GETs, 0.94 | 540, 12.05 |

- **50 GitOps requests a steady tick, against 540**, with every chart still
  resolved: 20 + 20 + 10 pages of 50. The first tick made the same 50. The
  500 OCIRepositories took 10 list requests (0.34 s summed) where the 500
  GETs were sequential. The tick's other 31 requests are #228's (above).
- The four steady ticks took 7.2 to 7.8 s and 5.2 to 5.4 CPU-seconds,
  against 12.8 s and 6.2 at `735751d`; the two runs had different code
  besides #248 (#226 and #228) and different host load, so the difference
  is not #248's alone.
- The HelmRelease lists read 1.18 MiB here and 3.65 MiB in the earlier run
  for the same 1,000 seeded HelmReleases; why was not investigated (the
  request counts do not depend on it).

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

### What grows with what

Everything is linear; nothing was superlinear. Most lists are paged at 500
objects, so a tick makes ceil(N / 500) requests per resource; the pod and
node lists, after a first page of 500, take as many objects a page as fit 8
MiB at the size of the largest object of the page before, at most 1,000
(#228). At full size at `9810fb6` that is 16 pod requests (`kube-system`'s
4,009 pods for the version check, 5, then the other namespaces' 10,000, 11),
3 for nodes, 3 for the metadata list of Helm Secrets, and 9 constant
(`/version`, the `kube-system` namespace, `/metrics`, the metadata lists of
Helm ConfigMaps, CRDs, IngressClasses and namespaces, and one GET and one
status UPDATE of the agent's `ClusterReadiness`): 31 in all (`59d8561`,
with pages of up to 2,000: 10 + 2 + 3 + 9 = 24). A tick that asks API
discovery again (the first, and the staleness rules above) adds 4 (7 with
the Argo CD and Flux CRDs installed), and a first tick one GET per Helm
release. With Argo CD and Flux objects every tick also lists them, and
the OCIRepositories their chartRefs name, at 50 a page (50 requests at
1,000 Applications and 1,000 HelmReleases, [measured at
`da90a86e`](#the-ocirepositories-listed-248); before #248 one GET per
OCIRepository instead, [measured on
`735751d`](#re-measured-on-main-with-and-without-argo-cd-and-flux)). A
cluster with no Helm release (the empty rows: 22 requests before #228)
also lists Deployments, StatefulSets and DaemonSets, metadata only, to look
for the tracking labels of Argo CD and Flux.
[Architecture](../architecture.md#api-cost-per-tick) states the formula.

The older tables say what the same fill cost before #232, #226 and #228:
61 requests, 38 of them pods, because the `kube-system` pods were listed
twice (#227).

### CPU and the chart limit

The tick is CPU-bound in the agent: 3.0 CPU-seconds when steady and 23.5 on
the first tick at full size on main `735751d` (the benchmark process also
runs its byte counter, a small share; at #228's code 3.0 and 22.0, above).
The chart's limit for the agent
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

All on main `735751d` (before #226 and #228), 4 October 2026, 04:13 to
05:42 UTC; the benchmark process outside a quota took 36.7 s and 23.5
CPU-seconds for the first tick and 3.4 to 4.7 s and 2.8 to 3.2 CPU-seconds
for a steady one (table above).
The peak RSS of the agent process was 66 to 71 MiB in every pod, and the
cgroup's peak memory (page cache included) 98 MiB at most, against the
256Mi limit.

These runs predate the tick reserve
([#238](https://github.com/abd-ulbasit/upgradescope/issues/238)):
collection now gets the tick deadline minus 30 seconds, 4m30s of the
5 minutes at the default interval, and each step's share shrinks with it.
The Helm step, given 59 s in these runs, would now get the 270 s left less
the first step's time, over the five steps left: at most 54 s (computed,
not measured). Compare a new run's step-deadline reasons against that, not
against 59 s.

- **At 200m the first tick did not finish its Helm step.** The tick used
  12.0 CPU-seconds in its 73 s, against the 14.7 that 0.2 CPU allows over
  that time, and left 132 releases unread; the status said so (`helm
  (partial)`, the first release not read, "step deadline: gave up after
  59s"). The following tick completed the step, in 105 s, using 13.0
  CPU-seconds, more than 132 releases explain (about 3 for them, 3 for the
  rest): the tick after a partial one seems to decode again some of what the
  partial one had decoded. That is not investigated here
  ([#247](https://github.com/abd-ulbasit/upgradescope/issues/247)). A steady tick took
  21 s against the 4 s it takes without a quota, throttled in 58 to 80% of
  the CFS periods it ran in.
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
GET each), and it must finish within its deadline. That was 59 s in these
runs; since the tick reserve it is at most 54 s ([above](#cpu-and-the-chart-limit)):
54 / 28.3 x 1,001 is about 1,910, so "about 1,900 releases" (about 2,000
with the 59 s before the reserve). It is not the 23.5 CPU-seconds per 1,000
releases of the first tick as a whole (that is CPU, not the step's wall
time, and the whole tick took 35 s). The Argo CD and Flux reads run inside
the same step and so share its 54 s: they added 8.7 s to a steady tick with
no quota (12.8 s against 4.1 s, [above](#re-measured-on-main-with-and-without-argo-cd-and-flux))
and 3.2 CPU-seconds, so with them the same arithmetic gives
(54 - 8.7) / 28.3 x 1,001, about 1,600 releases at 1,000 Applications and
1,000 HelmReleases (about 1,750 before the reserve). Both assume the step
scales with the number of releases and that a slower apiserver does not
stretch it, and neither was run, nor was any run made with the reserve.
Beyond that, give the agent more CPU or a longer interval (`agent.interval`,
whose half is the tick timeout; collection gets that less the reserve, and
the Helm step a fifth of what is left of it after the first step,
`runSteps`, so a longer interval also lengthens the step's deadline).

**#226 under a quota** (computed from the runs above, not measured: no pod
ran #226's code). #226 overlaps the GETs' waiting with the decoding, which
stays one release at a time; it does not make the decoding cheaper (22.0 to
22.8 CPU-seconds for a first tick at its commits, 23.1 to 23.5 on
`735751d`). Under a quota the CPU is the bound, so it helps little or not
at all:

- At 200m, main's first tick stopped at the Helm step's deadline with 132
  of 1,001 releases unread, having used 12.0 of the 14.7 CPU-seconds the
  quota allowed in its 73 s. Decoding 1,000 releases takes about 19 to 20.5
  CPU-seconds (a first tick's 22 to 23.5 less a steady tick's 3), more than
  the 11.8 that 0.2 CPU gives in the step's 59 s, so with #226 the step
  would still give up, with about as many releases unread.
- At 1 CPU, main's first tick completed in 35.2 s, throttled in 5% of its
  periods, so the core was mostly free while the GETs waited: #226 can save
  at most that waiting, 13.4 s in all for 1,000 GETs beside the apiserver
  (the recorder's figure for `735751d`, above), and less once the decoding
  has the core to itself. The step's ceiling of about 1,900 releases above
  is computed for the sequential fetch, so it is a lower estimate for #226's.

### Hotspots

| What | Found | Status |
|---|---|---|
| One GET per Helm release on every tick: 1,000 of 1,061 requests, 90 MiB, 33 s and 23.5 CPU-seconds at full size | The only cost that grew with releases, not pages | **Fixed**: the agent keeps what it decoded, keyed by the storage object's UID and resourceVersion; a steady tick makes none (61 requests then, 53 on `735751d` and 31 after #228). `TestHelmCache…` and `TestTicksFetchHelmReleasesOnlyWhenTheyChange` |
| The same 1,000 GETs on the first tick after a start, and on every one-shot `scan` | Sequential, 36.1 to 36.7 s and 23.1 to 23.5 CPU-seconds here; at a 60 ms round trip the first tick reached the Helm step's deadline with 231 releases unread; as a pod at the old 200m default it reached it with 132 unread (measured, [above](#cpu-and-the-chart-limit)) | **Fixed** for the round trip ([#226](https://github.com/abd-ulbasit/upgradescope/issues/226)): 8 GETs in flight, decoded one at a time in order; 42.4 s here at `9810fb6`, on a host whose load rose to 15 (27.1 s at the draft `59d8561`, at a load of 7 to 12), and at 60 ms 30.4 s at `06cdf7a` and 29.6 s at `5da764e`, with every release read. Decoding (22 CPU-seconds) and the client's rate limit (14 s for 1,000) are the bounds now, so under a CPU quota #226 does not help at 200m and saves at most the 13.4 s the GETs waited at 1 CPU (computed, [above](#cpu-and-the-chart-limit)); the chart's default limit is now 1 CPU, at which main's first tick read all 1,000. `TestCollectHelmConcurrentMatchesSequential` |
| `kube-system` pods are listed in full twice a tick: by the control-plane version check, then again by the all-pods list | 4,009 pods here; the second listing is not a separate 9 requests but 9 extra ones (those pods already fall inside the 29 pages of the all-pods list) and about 4,000 pods decoded twice; grows with nodes (every node adds a `kube-proxy` and a CNI pod) | **Fixed** ([#227](https://github.com/abd-ulbasit/upgradescope/issues/227), #232): the add-ons take them from the version check's list; 30 pod list requests a tick instead of 38 in the runs on `735751d` |
| Argo CD Applications, Flux HelmReleases and the OCIRepositories their chartRefs name are listed whole (page size 50) every tick (#218; the OCIRepositories since #248, a GET each before) | **Measured** (`BENCH_GITOPS=1`, at 1,000 Applications and 1,000 HelmReleases with 500 chartRefs): at `da90a86e` 50 of the 81 requests of a steady tick (20 + 20 + 10 lists), 9.0 MiB of 59.8, every chart resolved ([above](#the-ocirepositories-listed-248)); on main `735751d`, before #248, 540 of 596, 500 of them sequential OCIRepository GETs, 12 MiB of 63, 8.7 more seconds and 3.2 more CPU-seconds, under host noise ([above](#re-measured-on-main-with-and-without-argo-cd-and-flux)) | Fixed by [#248](https://github.com/abd-ulbasit/upgradescope/issues/248): a list instead of a GET per OCIRepository, a GET by name only where the list is forbidden; the rest grows by one request per 50 objects |
| The all-pods list is the steady tick's largest cost | 38 of 61 requests (30 of 53 after #232), 79% of the bytes; whole pod objects are needed for their images, and the agent keeps no watch | **Requests fixed** ([#228](https://github.com/abd-ulbasit/upgradescope/issues/228)): pod and node pages sized by their largest object (at most 1,000), discovery kept between ticks, the agent's object read once: 53 to 31 requests, short of the target of under 25. The bytes (50 MiB) and CPU (3 s) are unchanged, since every pod is still decoded every tick; reading pods less often (#228's option b) would cut those and the pod requests, and is not done |

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
The agent's report has the per-tick table, the GitOps table when the run
had the GitOps fill, and the requests, response bytes and time by verb and
resource of a steady tick and of the first, at the last fill level.
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
- The pod runs and the GitOps runs are of main `735751d`. The code of #226
  and #228 ran only in the benchmark process, outside a quota and without
  Argo CD or Flux objects: what it does under the chart's limits, and its
  larger pod pages' memory in a pod, are computed from those runs, not
  measured.
- The server benchmark generates inventories: real ones differ in the size
  of their Helm releases and the findings they carry. The per-snapshot
  storage scales with the inventory.
- The 60 ms round trip is a delay netem adds to what lab A's control plane
  sends: latency only, no bandwidth limit and no loss. The fake apiserver of
  `TestCollectHelmColdFetchAtRoundTrip` adds it as a sleep before each
  answer, and serves generated releases, so the decoding there is a laptop's
  and not the lab's.
