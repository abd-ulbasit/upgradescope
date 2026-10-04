# Scale and cost

What the agent costs a Kubernetes API server on a large cluster, and what
one `serve` can take from a fleet. Every table here comes from the final
runs of one session on 4 October 2026, between 04:30 and 05:00 in UTC+5
(the agent "before" table at 04:32, the agent "after" table at 04:45, the
server table at 04:58; the raw result files are stamped in UTC, which is the
evening of 3 October). The harness is in `hack/bench/`; you can run it
against your own lab to get your own numbers. Read [What is simulated](#what-is-simulated) before you quote
them.

## The short answers

- **A steady agent tick on a cluster of 2,001 nodes, about 14,000 pods and
  1,000 Helm releases made 61 API requests** and read 68 MiB of responses
  (4.3 MiB on the wire, compressed). It took 5.6 s and 3.6 CPU-seconds, with
  a peak live heap of 27 MiB and a peak RSS of 54 MiB. At the default
  interval of 10 minutes that is 0.1 requests a second.
- **The first tick after the agent starts costs more**: it reads each Helm
  release once, 1,061 requests in all, 36 s and 24 CPU-seconds here. Before
  [#71](https://github.com/abd-ulbasit/upgradescope/issues/71) every tick
  cost that, because the agent fetched every release again each time: it was
  the one cost that grew with releases rather than with pages (1,000 GETs of
  1,061 requests).
- **Memory fits the chart's defaults** (64Mi request, 256Mi limit) with room:
  the peak RSS never passed 59 MiB at any size measured, the first tick
  included. CPU is what a large cluster uses: see
  [CPU](#cpu-and-the-chart-limit).
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
| upgradescope | the agent runs: main `a3e72ea` (#218, GitOps charts) plus this work; the "before" table is the same tree with the Helm step given no cache, which is what #71 changed. The server runs: main `f195ba5` plus this work (the server code is unchanged by this work; #217 is examples and tooling). The branch was rebased between the two, so its commit hashes name neither tree exactly. The numbers include #218's per-tick discovery and workload requests, but **its Argo CD and Flux lists are not measured**: the lab has neither tool's CRDs, so those lists are never made (see [Hotspots](#hotspots)) |
| Also running | two other idle kind clusters on the same ThinkPad, and for the agent runs the lab's own KWOK controller keeping 2,000 nodes alive |

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

### What grows with what

Everything is linear; nothing was superlinear. Each list is paged at 500
objects, so a tick makes ceil(N / 500) requests per resource: 38 are pod
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

The tick is CPU-bound in the agent (the process also runs the benchmark's
byte counter, a small share): 3.6 CPU-seconds in 5.6 s of wall time when
steady, 24.4 in 36.1 when it reads 1,000 releases. The chart's default limit
for the agent is 200m (`agent.resources.limits.cpu`), which allows 0.2
CPU-seconds a second, so those ticks last at least 18 s and 2 minutes there
(computed from the CPU time, not measured under a cgroup quota; measuring it
is [#229](https://github.com/abd-ulbasit/upgradescope/issues/229)). The tick
deadline is half the interval, 5 minutes at the default, and the Helm step,
the second of six, gets a fifth of the time left, about a minute, so a first
tick of 1,000 releases at 200m will probably reach the step's deadline and
leave some releases unread (a gap the report names); the next tick reads the
rest, because the cache keeps what was decoded. On a cluster this size, give
the agent 500m to 1 CPU, or expect its first ticks to be partial.

### Hotspots

| What | Found | Status |
|---|---|---|
| One GET per Helm release on every tick: 1,000 of 1,061 requests, 90 MiB, 33 s and 23.5 CPU-seconds at full size | The only cost that grew with releases, not pages | **Fixed**: the agent keeps what it decoded, keyed by the storage object's UID and resourceVersion; a steady tick makes none (61 requests, 5.6 s). `TestHelmCache…` and `TestTicksFetchHelmReleasesOnlyWhenTheyChange` |
| The same 1,000 GETs on the first tick after a start, and on every one-shot `scan` | Sequential, 36 s and 24 CPU-seconds here; reaches the step deadline at 200m or over a slow link | Open: bounded-concurrency fetching ([#226](https://github.com/abd-ulbasit/upgradescope/issues/226)) |
| `kube-system` pods are listed in full twice a tick: by the control-plane version check, then again by the all-pods list | 4,009 pods here; the second listing is not a separate 9 requests but 9 extra ones (those pods already fall inside the 29 pages of the all-pods list) and about 4,000 pods decoded twice; grows with nodes (every node adds a `kube-proxy` and a CNI pod) | Open ([#227](https://github.com/abd-ulbasit/upgradescope/issues/227)) |
| Argo CD Applications and Flux HelmReleases are listed whole (page size 50) every tick, plus a GET per distinct OCIRepository (#218) | **Not measured**: the lab has neither tool installed, so the harness never makes these requests. ceil(N / 50) per tool grows faster than a metadata list (whole objects, small pages) | Open, #233 |
| The all-pods list is the steady tick's largest cost | 38 of 61 requests; whole pod objects are needed for their images, and the agent keeps no watch | Open: needs a decision on frequency ([#228](https://github.com/abd-ulbasit/upgradescope/issues/228)) |

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

# Server: SQLite, then Postgres in a throwaway container on the engine
# `docker` is bound to (BENCH_RUN_ON=lab-host runs the server beside it).
BENCH_RUN_ON=lab-host make bench-ingest
BENCH_BACKENDS=sqlite make bench-ingest   # no Docker needed
```

`hack/bench/agent.sh` documents its knobs (`BENCH_STEPS`, `BENCH_TICKS`,
`BENCH_HELM_REVISIONS`, `BENCH_RESET_CMD`, `BENCH_NO_HELM_CACHE`); both scripts print their tables
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
- The agent was not run under a cgroup CPU quota or a memory limit; the
  figures for the chart's limits are computed from CPU time and peak memory
  ([#229](https://github.com/abd-ulbasit/upgradescope/issues/229)).
- The server benchmark generates inventories: real ones differ in the size
  of their Helm releases and the findings they carry. The per-snapshot
  storage scales with the inventory.
