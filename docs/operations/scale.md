# Scale and cost

What the agent costs a Kubernetes API server on a large cluster, and what
one `serve` can take from a fleet. Measured on 3 October 2026 with the
harness in `hack/bench/`, which you can run against your own lab to get your
own numbers. Read [What is simulated](#what-is-simulated) before you quote
them.

## The short answers

- **A steady agent tick on a cluster of 2,001 nodes, about 14,000 pods and
  1,000 Helm releases made 59 API requests** and read 68 MiB of responses
  (4.3 MiB on the wire, compressed). It took 4.4 s and 3.4 CPU-seconds, with
  a peak live heap of 27 MiB and a peak RSS of 54 MiB. At the default
  interval of 10 minutes that is 0.1 requests a second.
- **The first tick after the agent starts costs more**: it reads each Helm
  release once, 1,059 requests in all, 35 s and 24 CPU-seconds here. Before
  [#71](https://github.com/abd-ulbasit/upgradescope/issues/71) every tick
  cost that, because the agent fetched every release again each time: it was
  the one cost that grew with releases rather than with pages (1,000 GETs of
  1,059 requests).
- **Memory fits the chart's defaults** (64Mi request, 256Mi limit) with room:
  the peak RSS never passed 59 MiB at any size measured, including the first tick. CPU is what a large
  cluster uses: see [CPU](#cpu-and-the-chart-limit).
- **One `serve` ingested 200 clusters with three targets each, all pushing at
  once, with no failed push and no retry**: 57 new snapshots a second on
  SQLite and 42 on Postgres, on a 2017 dual-core i3 that also ran the
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
| upgradescope | branch `feat/scale-harness` at `91a4e74` for the agent (the "before" column is the same commit with the Helm cache commit `e1ef4ca` reverted) and `a735fcb` for the server; the server code is unchanged by this work |
| Also running | two other idle kind clusters on the same ThinkPad, and for the agent runs the lab's own KWOK controller keeping 2,000 nodes alive |

### What is simulated

- **The nodes are fake.** KWOK makes 2,000 `Node` objects report Ready with
  a kubelet version and marks the pods placed on them Running. No kubelet,
  container runtime or CNI exists, so the apiserver does none of the work
  real kubelets cause (heartbeats from 2,000 machines, status patches of
  real pods, watches). KWOK's own heartbeats keep the control-plane
  container busy: 1.1 to 2.7 cores and 2.8 to 3.0 GiB at full size, before
  the agent's ticks.
- **There is one apiserver, one etcd member, and a kind control plane.** No
  HA control plane, no load balancer in front, no priority-and-fairness
  contention with other clients, no admission webhooks, and the agent talks
  to it from the same host (a round trip well under a millisecond). The
  requests of a tick are mostly sequential, so a slow link multiplies: a
  trial run from a laptop over a VPN (60 ms round trip) took 15 s for the
  tick that took 5.4 s beside the apiserver (250 releases), and an earlier
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
first; the first, which fills the Helm cache, is beside it.

Full size is 2,001 nodes (2,000 fake and the control plane), 10,000 seeded
pods plus the 4,000 DaemonSet pods, 6,000 ConfigMaps, 4,000 Deployments, 100
namespaces and 1,000 Helm release Secrets.

**Steady tick, after the fix** (Helm releases fetched only when new or
changed):

| Fill | Nodes | Helm releases | Requests | Of them LIST pods | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB | First tick: requests, wall s, CPU s |
|---|---|---|---|---|---|---|---|---|---|---|---|
| empty | 1 | 0 | 17 | 2 | 4.0 | 0.4 | 0.5 | 0.4 | 22.9 | 55.2 | 17, 2.5, 0.4 |
| 1/4 | 501 | 250 | 27 | 11 | 19.8 | 1.5 | 1.3 | 1.1 | 23.3 | 58.5 | 277, 5.6, 5.4 |
| 1/2 | 1,001 | 500 | 38 | 20 | 36.0 | 2.4 | 2.0 | 1.8 | 25.0 | 49.4 | 538, 12.9, 11.5 |
| full | 2,001 | 1,000 | 59 | 38 | 67.9 | 4.3 | 4.4 | 3.4 | 27.1 | 54.4 | 1,059, 35.1, 24.4 |

**The same ticks before the fix** (a GET per release on every tick):

| Fill | Requests | GET Secrets | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB |
|---|---|---|---|---|---|---|---|---|
| empty | 17 | 0 | 3.9 | 0.4 | 0.5 | 0.4 | 22.7 | 55.8 |
| 1/4 | 277 | 250 | 25.0 | 6.3 | 5.3 | 5.3 | 20.5 | 57.2 |
| 1/2 | 538 | 500 | 47.3 | 12.8 | 13.3 | 11.4 | 25.2 | 51.7 |
| full | 1,059 | 1,000 | 90.1 | 25.1 | 29.2 | 23.8 | 26.4 | 56.8 |

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
| GET | `clusterreadinesses`, API discovery | 2 each |
| CREATE | `clusterreadinesses` | 1 |
| GET | `/metrics`, `/version`, `kube-system` namespace | 1 each |
| LIST | configmaps (metadata), CRDs, IngressClasses, namespaces | 1 each |
| UPDATE | `clusterreadinesses/status` | 1 |

### What grows with what

Everything is linear; nothing was superlinear. Each list is paged at 500
objects, so a tick makes ceil(N / 500) requests per resource: 38 are pod
requests (14,000 pods in all namespaces, then 4,000 in `kube-system`), 5 are
nodes, 3 are the metadata list of Helm Secrets. The rest is constant, about
15 requests. [Architecture](../architecture.md#api-cost-per-tick) states the
formula.

### CPU and the chart limit

The tick is CPU-bound in the agent (the process also runs the benchmark's
byte counter, a small share): 3.4 CPU-seconds in 4.4 s of wall time when
steady, 24.4 in 35.1 when it reads 1,000 releases. The chart's default limit
for the agent is 200m (`agent.resources.limits.cpu`), which allows 0.2
CPU-seconds a second, so those ticks last at least 17 s and 2 minutes there
(computed from the CPU time, not measured under a cgroup quota). The tick
deadline is half the interval, 5 minutes at the default, and the Helm step,
the second of six, gets a fifth of the time left, about a minute, so a first
tick of 1,000 releases at 200m will probably reach the step's deadline and
leave some releases unread (a gap the report names); the next tick reads the
rest, because the cache keeps what was decoded. On a cluster this size, give
the agent 500m to 1 CPU, or expect its first ticks to be partial.

### Hotspots

| What | Found | Status |
|---|---|---|
| One GET per Helm release on every tick: 1,000 of 1,059 requests, 90 MiB, 29 s and 24 CPU-seconds at full size | The only cost that grew with releases, not pages | **Fixed**: the agent keeps what it decoded, keyed by the storage object's UID and resourceVersion; a steady tick makes none (59 requests, 4.4 s). `TestHelmCache…` and `TestTicksFetchHelmReleasesOnlyWhenTheyChange` |
| The same 1,000 GETs on the first tick after a start, and on every one-shot `scan` | Sequential, 35 s and 24 CPU-seconds here; reaches the step deadline at 200m or over a slow link | Open: bounded-concurrency fetching, see the follow-up in the pull request |
| `kube-system` pods are listed in full twice a tick: by the control-plane version check, then again by the all-pods list | 4,009 pods here, 9 requests each time; grows with nodes (every node adds a `kube-proxy` and a CNI pod) | Open |
| The all-pods list is the steady tick's largest cost | 38 of 59 requests; whole pod objects are needed for their images, and the agent keeps no watch | Open: needs a decision on frequency, see the follow-up |

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
`pg_database_size` (Postgres) over the new snapshots.

| Backend | Pushers | Round | Pushes/s | p50 ms | p99 ms | CPU ms/push | Peak heap MiB | DB KiB/snapshot |
|---|---|---|---|---|---|---|---|---|
| SQLite | 1 | new | 52.5 | 10.5 | 99.7 | 25.8 | 8 | 105.6 |
| SQLite | 1 | changed | 51.9 | 13.0 | 100.3 | 25.9 | 9 | 98.7 |
| SQLite | 1 | duplicate | 162.9 | 4.5 | 27.2 | 7.3 | 6 | 0 |
| SQLite | 16 | new | 54.7 | 298.5 | 361.9 | 25.2 | 11 | 105.6 |
| SQLite | 16 | changed | 53.6 | 302.6 | 376.3 | 25.6 | 12 | 99.1 |
| SQLite | 16 | duplicate | 177.7 | 88.9 | 114.7 | 7.1 | 9 | 0 |
| SQLite | 200 | new | 57.4 | 1,531 | 3,352 | 23.4 | 33 | 103.1 |
| SQLite | 200 | changed | 57.1 | 1,611 | 3,477 | 23.8 | 33 | 97.6 |
| SQLite | 200 | duplicate | 173.1 | 574 | 1,134 | 7.3 | 26 | 0 |
| Postgres | 1 | new | 37.4 | 17.7 | 122.6 | 27.1 | 10 | 19.6 |
| Postgres | 1 | changed | 37.9 | 17.3 | 119.7 | 27.5 | 9 | 18.9 |
| Postgres | 1 | duplicate | 105.0 | 7.5 | 31.2 | 7.7 | 8 | 0 |
| Postgres | 16 | new | 40.1 | 399 | 485 | 26.8 | 11 | 19.6 |
| Postgres | 16 | changed | 40.4 | 404 | 478 | 27.1 | 11 | 18.8 |
| Postgres | 16 | duplicate | 117.2 | 134 | 161 | 7.7 | 10 | 0 |
| Postgres | 200 | new | 41.5 | 2,296 | 4,587 | 25.4 | 32 | 19.4 |
| Postgres | 200 | changed | 41.9 | 2,286 | 4,616 | 25.6 | 33 | 18.6 |
| Postgres | 200 | duplicate | 117.7 | 840 | 1,652 | 7.8 | 29 | 0 |

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
  every tick, that is under 1% of the 57 a second measured. The bottleneck of
  a fleet is the dashboard's reads, not ingest: see
  [Running the server](../operations.md#cpu-and-fleet-read-latency).
- **Postgres grows 5 times more slowly**: 19 KiB a snapshot against about
  100 KiB on SQLite for the same snapshots and their three evaluations each.
  Both keep the inventory and the reports as binary blobs; SQLite stores
  them as they are and Postgres compresses values over 2 KiB. Plan storage
  with [Retention and backup](retention-and-backup.md#retention-and-sizing):
  200 clusters whose inventory changes four times a day, kept 90 days, are
  72,000 snapshots, about 7 GiB on SQLite and 1.3 GiB on Postgres at these
  inventory sizes.
- **Postgres took about 30% less throughput here** (42 against 57 pushes a
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
`BENCH_HELM_REVISIONS`, `BENCH_RESET_CMD`); both scripts print their tables
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
  figures for the chart's limits are computed from CPU time and peak memory.
- The server benchmark generates inventories: real ones differ in the size
  of their Helm releases and the findings they carry. The per-snapshot
  storage scales with the inventory.
