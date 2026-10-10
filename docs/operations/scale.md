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

- **With the pod pass reused (the default since
  [#228](https://github.com/abd-ulbasit/upgradescope/issues/228)'s option
  b), a steady agent tick at 2,001 nodes, about 14,000 pods and 1,000 Helm
  releases makes 20 API requests on 2 ticks of 3, and 31 on the third.**
  `--pod-pass-every` (3 by default) and `--pod-pass-max-age` (1h) make the
  agent list the pods outside `kube-system` once per 3 ticks, or sooner when
  that pass is an hour old, and detect add-ons from its images and labels in
  between. Measured at `0b6fec28` (this branch on main `f121698a`),
  10 October 2026, 02:56 to 03:12 UTC, on lab A: the 5 ticks that reused the
  pass made 20 requests, read 27.8 MiB (1.5 MiB on the wire) in 2.4 s and 1.8
  CPU-seconds (medians); the 2 that listed every pod made 31, read 50.4 MiB
  (3.3 on the wire) in 4.4 s and 3.4 CPU-seconds. The requests are those
  of the earlier run at `9810fb6` (below); its time and CPU are not
  comparable, because this run was made with the ThinkPad's load average
  at 8.5 to 11.9 (4 threads), and that run took 3.7 s and 3.0 CPU-seconds.
  The wire bytes (3.3 MiB against 3.1) differ by fill, not by load: run 2
  of the same day, on the same fill with `--pod-pass-every=1`, read 3.3 MiB
  too (3.1 on one tick). The numbers were measured before `8ca7e786` (a
  change in the Helm releases or GitOps charts that name an add-on forces a
  pass) and before `81761afd` (a tick that lists every pod drops the held
  pass before its first list) and still hold at `daf513a4`, the head of the
  branch before this follow-up (checked, not re-measured). Of the commits in between, only
  those two change what a tick does; the others are documentation, the
  agent's start-up warning and the dashboard. For `8ca7e786`: the fill's
  charts (`chart-rel-NNNN`) name no registry add-on, so the signature
  that trigger compares is empty on every tick and the requests are
  unchanged (checked from the seed, `hack/bench/seed/helm.go`, and the
  registry). For `81761afd`, which moves the old pass's release from after
  the first pod list to before it: at this fill the held pass is about
  300 distinct (namespace, image) pairs (pod i is in namespace i%100 and
  runs image i%12, so the pairs repeat with period lcm(100, 12) = 300), plus 4
  pairs of add-on images and 4 labelled pods of the add-on pods (every 500th
  pod, all in `bench-ns-000`), so a few tens of KiB. Holding it during the
  list or not cannot move the peak heap (30.5 and 36.4 MiB) or the peak RSS
  (63.6 MiB) by anything the 5 ms sampler could see, nor a request. The mean over a
  cycle of 3 is 23.7 requests, 35.3 MiB and 2.3 CPU-seconds (computed from
  those medians, not measured). The price is staleness: an add-on installed
  or upgraded right after a full pass, other than through Helm or a GitOps
  chart reference (a change in those makes the next tick a full pass), is
  reported as it was for at most 2 ticks (about 20 minutes at the default
  interval), and a pass that is `--pod-pass-max-age` old is not reused; the
  report, the ClusterReadiness status and
  `upgradescope_addon_evidence_age_seconds` say how old the evidence is.
  [The pod pass every Nth tick](#the-pod-pass-every-nth-tick-228) has the
  tables and the rules. The paragraphs below are the tick that lists every
  pod (`--pod-pass-every=1`, and one in 3 by default).
- **A steady agent tick that lists every pod, on a cluster of 2,001 nodes,
  about 14,000 pods and 1,000 Helm releases, makes 31 API requests** at the
  code of
  [#228](https://github.com/abd-ulbasit/upgradescope/issues/228) (`9810fb6`,
  4 October 2026, 06:55 to 07:03 UTC), which misses #228's target of under
  25: the ticks that reuse the pass (20, measured) and the mean of a cycle
  of 3 (23.7, computed) are under it, the tick that lists every pod is
  not. It reads 50.4 MiB of responses (3.1 MiB on the wire,
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
  requests cost that. As a pod at that code (`da90a86e`, at 200m, 9
  October 2026) the agent's peak RSS was 81 MiB and the cgroup's peak
  memory 107 MiB ([#247's run](#the-tick-after-a-partial-helm-step-247)). That is above the 64Mi
  request, which only informs scheduling. A pod page is bounded by its count
  only, at most 1,000 pods, so its worst case is 1,000 times the largest
  pod, whatever its size, reached when small pods are followed by large
  ones. 1,000 pods of about 40 KiB after small ones held 122.3 to 125.3 MiB
  of live heap in a test on GitHub's ubuntu-latest runner
  (`TestPodPagePeakHeapIsBounded`, which allows 144 MiB); at that rate,
  about 3.2 bytes of live heap per encoded byte, pods of about 60 KiB after
  small ones would take one page past the agent's `GOMEMLIMIT` (230.4 MiB)
  beside what the rest of the agent holds, and likely past the limit, where
  the pages of 500 before #228 would have reached it at about 120 KiB
  (computed, not measured; see [the steady tick](#the-tick-after-226-and-228)).
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
| upgradescope, #247 | `da90a86e` as a pod at 200m (the image packed as above, version `collect-da90a86e`), 9 October 2026, 17:46 to 18:12 UTC, on lab `us-lab-137b` reset and filled by `BENCH_STEPS=1 BENCH_TICKS=2` at `86500c36` (docs only after `da90a86e`) just before; the ThinkPad's kernel was 7.0.0-38 then ([the run](#the-tick-after-a-partial-helm-step-247)) |
| Also running, #247 | `bookstore` and lab `us-lab-137`, both idle (11.7 to 24.3% of a core each; `us-lab-137` at 581 to 664 MiB, no fill); host load average 4.8 to 9.5 |
| upgradescope, #247 (the cause) | `9687cffc` (this branch: PR #271's tip `0eb41cab`, which is `da90a86e` and 18 commits since, changing the GitOps reads, the tick reserve, the wording of the step's reasons and the push but not the Helm fetch, cache or decoding, plus the opt-in `--pprof-addr`), as a pod at 200m, the image packed as above (version `prof247-before`, `linux/amd64`, `CGO_ENABLED=0 -trimpath -s -w`), 9 October 2026, on lab `us-lab-137b` reset and filled by `BENCH_STEPS=1 BENCH_TICKS=2` at `9687cffc` (21:40 to 21:48 UTC). Two pod runs, each a fresh pod and so an empty cache: with the profiler on and CPU profiles taken around each tick, 22:10 to 22:36 UTC, and with it off, 22:40 to 23:05 UTC. A first attempt at 21:56 UTC, eight minutes after the fill, was dropped: the control plane was still busy (kube-apiserver at 318% of a core and 1.2 GiB in `top` at 22:06, the `kindnet` and `kube-proxy` DaemonSets 1,971 and 1,980 ready of 2,001), the kubelet killed the agent for a failed liveness probe about 5 minutes after it started, and the tick after the restart could not write its status ([the runs](#the-tick-after-a-partial-helm-step-247)) |
| upgradescope, #228 (pod pass) | `0b6fec28` (this branch on main `f121698a`, which has #251, #262, #271, #279 and #286), the benchmark's `TestBenchAgentTick` cross-compiled for `linux/amd64` and run on the ThinkPad beside the apiserver (`BENCH_RUN_ON`, outside a CPU quota), 10 October 2026, 02:56 to 03:12 UTC, under the measurement lock, on lab A (`us-lab-137`, reset first, filled by `BENCH_STEPS=1`, settled 150 s, Kubernetes 1.37.0), no Argo CD or Flux objects. Run 1: `BENCH_TICKS=8`, the default `--pod-pass-every=3`; run 2, on the same fill: `BENCH_TICKS=5 BENCH_POD_PASS_EVERY=1`. The lab was reset afterwards ([the run](#the-pod-pass-every-nth-tick-228)) |
| Also running, #228 (pod pass) | lab B (`us-lab-137b`) up and idle, with no fill: its control plane at 11.6 to 21.9% of a core and 569 to 749 MiB in the five `docker stats` taken (before the reset, before each run's ticks, after run 1 and after run 2), and `bookstore` at 10.8 to 19.9% of a core and 567 to 617 MiB; lab A's own control plane at 185 to 315% of a core and 3.2 GiB (KWOK's heartbeats) when sampled at the fill. The ThinkPad's 1-minute load average was 0.45 before the reset, 8.5 to 11.9 around run 1 and 9.4 to 22.5 around run 2 (4 threads) |
| Also running, #247 (the cause) | `bookstore` and lab `us-lab-137`, idle (10 to 17% of a core each; `us-lab-137` at 575 to 680 MiB, no fill; `docker stats` at the start and end of each run and around the decode runs); the lab's own control plane at 125 to 325% of a core from KWOK's heartbeats; the host's 1-minute load average 3.1 to 11.2 in the spot samples between 21:48 and 23:05 UTC (1.25 before the fill, at 21:40). The two decode runs below (the collector's `TestHelmDecodeCostByRelease`, outside a quota, the lab's agent uninstalled) ran on the ThinkPad at 23:08 UTC at a load of 4.6 and 7.2; an earlier one at 22:36 UTC (pod idle, load 7.3 to 10.7, before the test reported 43, 51 and 83) gave 12.7 s for all 1,000 and 5.40 s for the last 50 |
| upgradescope, #285 | before: main `4b1c2893`; after: `572f0e47` (this branch's code; the commits after it change docs only). `TestHelmDecodeCostByRelease` cross-compiled for `linux/amd64` (`GOOS=linux GOARCH=amd64 go test -c ./internal/collect`) and run on the ThinkPad, outside a CPU quota, 10 October 2026, 10:13 to 10:18 UTC, under the measurement lock: four runs of each binary, alternating, each the test's own mean of 3 passes over the 1,000 payloads (an earlier set of four pairs, on the first version of the change, `db5b0845`, ran 09:55 to 10:00 UTC and read within 0.08 s of these means). The payloads are the bench fill's Helm Secrets, read from lab A (`us-lab-137`, kind 1.37.0) after `hack/bench/seed` created only them (`--nodes 0 --pods 0 --configmaps 0 --deployments 0 --helm-releases 1000 --wait 0`, seed 1, 09:20 UTC; 22,841,288 bytes stored and 106,874,938 decompressed, 800 small, 150 medium and 50 large, the totals [given above](#what-is-simulated)); the lab was reset right after. The heap figures are the same tests run on the Mac, at a load average of 31 to 68 ([the runs](#a-manifest-document-parsed-once-285)) |
| Also running, #285 | lab B (`us-lab-137b`) was idle, as the work package said; this run did not query it. The ThinkPad's 1-minute load average was 0.88 to 1.68 at the start of each of the eight timed runs of 10:13 to 10:18 UTC (each run is itself one busy thread of the four), and 1.45 to 2.20 in the earlier set: a fill would have shown 2.4 to 3.6 threads on top. |

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
  (compare the real charts measured in `internal/collect/helm.go`). The
  class follows the release's number (`rel-NNNN`: the large ones are those
  whose number ends in 95 to 99) and so does its namespace (`bench-ns-NNN`,
  one per last two digits of the number, so `bench-ns-000` to `bench-ns-099`
  with the seeder's default 100 namespaces), so **at the default 100
  namespaces and 1,000 releases the large releases are exactly the last five
  namespaces (`bench-ns-095` to `bench-ns-099`), the last 50 releases in the
  order the Helm step visits them** (namespace, then name): the ones a first
  tick that gives up leaves unread, and the costliest to decode (about 8 times a mean release,
  [measured below](#the-tick-after-a-partial-helm-step-247)). Every
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
  measures it on GitHub's ubuntu-latest runner (linux/amd64), where users'
  agents run (four CI runs at `23f8739` and `04c39fa`, runs 37950213827
  and 37964795504, this change before its rebase onto `377fd77`; 9 October
  2026): 500 pods of 137 bytes, then 1,000 of up to 41,685 bytes (39.4
  MiB encoded), held between 122.3 and 125.3 MiB of live heap (its lower bounds 122.3 to
  123.4, its upper bounds 125.0 to 125.3), under half the chart's 256Mi; a
  run of those pods, which stays at 500 a page as before, 61.7 to 68.0 MiB;
  production-sized pods (about 8 KiB in protobuf, managedFields included;
  about 990 a page) 32.9 to 34.3 MiB. A single reading after a forced
  collection can also count the garbage of reading the response, up to
  153.8 MiB in those runs: #251's CI read 153.0 MiB and failed the 128 MiB
  the test enforced then, 2.5 to 3.5 MiB above an Apple M1 Pro's readings.
  So the test lists each case 10 times, passes on the lowest upper bound,
  and enforces 144 MiB. Five dispatched runs of the final test, at
  `443a542` (this change before its last rebase onto main; runs 37987299524, 37987318295, 37990925915, 37994320874 and
  37996739083, 9 October 2026), all passed it: the worst case between 123.0
  and 125.1 MiB (lower bounds 119.4 to 123.8, per attempt), a run of large
  pods 62.4 to 63.5, production-sized pods 33.0 to 33.4 (lower bounds); 8 of
  the 50 worst-case attempts read past 144 MiB, up to 153.7 (garbage), and
  every run had at least 6 of its 10 under it. 144 MiB is 18.7 MiB (14.9%)
  above the highest upper bound read, and 86.4 MiB under the agent's `GOMEMLIMIT` (90% of 256Mi, 230.4
  MiB) for the rest of the agent and a collection's garbage. The 40 KiB
  pods are an example, not the bound: at the rate they measured, about 3.2
  bytes of live heap per encoded byte (125.3 MiB for 39.4 MiB), one page of
  1,000 pods passes the agent's `GOMEMLIMIT` at pods of about 60 KiB after a
  page of small ones, beside the 39.2 MiB the rest of the agent's heap
  peaked at here ((230.4 − 39.2) MiB / 3.18 / 1,000 is 61.6 KiB; 74.2 KiB
  with nothing else held), where pages of at most 500, before #228, would
  have reached it at about 120 KiB (123.1 KiB; computed, not measured). Pods
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
**31 requests miss #228's target of under 25.** Pages of up to 2,000 (24
requests) have the worst case above, so they were not shipped; reading the
pods less often (#228's option b) cuts the pod requests and the bytes and
CPU with them, and is [the next section](#the-pod-pass-every-nth-tick-228).
The split after, at `9810fb6`, of a tick that lists every pod:

| Verb | Resource | Requests | Response MiB | Time s (summed) |
|---|---|---|---|---|
| LIST | pods | 16 | 40.10 | 1.09 |
| LIST | nodes | 3 | 5.32 | 0.12 |
| LIST | secrets (metadata) | 3 | 0.42 | 0.04 |
| GET | `/metrics` | 1 | 4.43 | 0.23 |
| GET | `/version`, `clusterreadinesses`, `kube-system` namespace | 1 each | 0, 0.01, 0 | 0.01 or less each |
| LIST | configmaps (metadata), CRDs, IngressClasses, namespaces | 1 each | 0, 0.08, 0, 0.04 | 0.02 or less each |
| UPDATE | `clusterreadinesses/status` | 1 | 0.01 | 0.04 |

### The pod pass every Nth tick (#228)

The pods are 16 of the 31 requests of a tick that lists every pod and 79%
of its bytes (the split above), decoded whole to read their images and
labels, and a watch or informer is excluded by design (AG-01). So the
agent lists the pods **outside `kube-system`** once per `--pod-pass-every`
ticks (3 by default, the full pass being the first of them), and in between
detects add-ons from the images and labels that pass read
(`collect.PodPassCache`, PF-18). It lists them again sooner when the last
full pass is `--pod-pass-max-age` old (1h by default; the chart's
`agent.podPassEvery` and `agent.podPassMaxAge`). `--pod-pass-every=1` lists
them every tick, as before.

What is reused is the evidence (the distinct namespace and image pairs and
labelled pods, never a verdict): the registry is applied to it on every
tick, so a knowledge-base update takes effect at once. Read on every tick
whatever the setting: the `kube-system` pods (the `versions` capability
lists them for the control-plane components and kube-proxy skew, and the
add-ons take their evidence from that list), Helm releases, Argo CD and
Flux charts, and IngressClasses. A full pass is forced when

- the cache holds no complete pass: the agent has just started, or the last
  pass failed or read only some of its pages (a failed or partial pass is
  never kept, and drops the evidence of an earlier one);
- the last full pass is `--pod-pass-max-age` old;
- `versions` did not list the `kube-system` pods on this tick, because the
  held pass does not hold them; and
- the Helm releases or the Argo CD and Flux chart references that name a
  registry add-on are not those the pass was taken with: a release or
  chart that appeared, went away or changed its chart, chart version,
  appVersion, revision or status. They are read on every tick anyway, so
  this early trigger costs no request. Releases of no add-on (your own
  applications) do not count, however often they are upgraded
  (`TestPodPassHelmUpgradeForcesAFullPass`, `TestPodPassIgnoresReleasesOfNoAddOn`,
  `TestPodPassGitOpsChartChangeForcesAFullPass`). Without it, a release
  upgraded across a release line would be joined with the old pods and
  reported as two installs for up to 2 ticks.

**Worst-case staleness.** An add-on installed or upgraded right after a full
pass began is reported as it was for the next `--pod-pass-every` minus 1
ticks (2 at the default: about 20 minutes at the default 10-minute
interval, with its jitter of up to 10%), and a tick does not reuse a pass
that is `--pod-pass-max-age` old or more, which bites first on a long
interval (at 30 minutes the default hour allows the first reuse, which is
a pass of about 30 minutes, and loses the second on the ticks the jitter
spaces widely; at an hour only the ticks the jitter brings early reuse the
pass, about half of the first ticks after one, and none does from an
interval of about 67 minutes, the hour over 0.9, a little less where a
tick takes time). The age is measured when the tick reuses the pass, and
that tick's report and ClusterReadiness status stay up until the next
one, so a viewer can see evidence up to about the maximum age plus one
interval old. An add-on removed or one whose last pod went away is
likewise reported until the next full pass.

**The maximum age needed.** Ticks are spaced by the interval with a jitter
of 10% either way (0.9 to 1.1 times it), and the sleep starts after a tick
ends, so each spacing also holds that tick's run time. The last reuse after
a pass is `--pod-pass-every` minus one spacings old, up to 1.1 times the
interval plus the run time of each. So `--pod-pass-max-age` must exceed
1.1 times `--interval` times (`--pod-pass-every` minus one), the floor, for
every one of those reuses to work, and the floor is needed but not
enough: the ticks' run time comes on top. At the default interval of 10
minutes and `--pod-pass-every 3` the floor is 22 minutes, and 21 fails
on the draws where the two spacings and their run time reach 21 minutes;
25 works unless the ticks take more than a minute and a half each (two
spacings of up to 11 minutes and two run times must stay under 25; a
reusing tick took 2.4 s here, a full one 4.4 s, the first 35 s). The default hour clears it. A
maximum age at or below `--interval` defeats the setting most: the
pass is about one interval old at the next tick, so every tick lists every
pod but one the jitter brings early. Between the interval and the floor the
first reuses work and the last ones depend on the draw. The agent logs a
warning at start for any maximum age at or below the floor, with the floor
in its `mustExceed` field, and the largest `--pod-pass-every` that fits the
maximum age you set in `podPassEveryThatFits` (`TestRunWarnsWhenPodPassMaxAgeIsTooShortForPodPassEvery`):
at a 30-minute interval the default hour fits 2, whose floor is 33 minutes,
not the default 3, whose floor is 66. A `--pod-pass-every` of billions
does not overflow the floor: it saturates at the largest duration, and the
warning is raised (`TestPodPassMaxAgeFloorSaturates`).

Which add-ons can be behind. Those in `kube-system` cannot: their pods are
read on every tick. A change made through Helm (a new release, or a new
revision of one, which an upgrade, a rollback and a values change all make)
or to an Argo CD or Flux chart reference that names the add-on (a new
`targetRevision`, chart or target namespace) forces a full pass on the next
tick (the early trigger above), so those are reported as they are. What
can be behind, for the 2 ticks: an add-on installed without Helm or a chart
reference; an image changed in place (`kubectl set image`, a patched
manifest); and an upgrade made by a GitOps tool through a version
constraint (`4.*`), which leaves the resource untouched.
The age of what a tick reused is `addOnEvidenceAgeSeconds` in the
inventory and the report (absent when every pod was read), the same on the
ClusterReadiness status, in the tick's log line, on the dashboard's Cluster view
(as "Pod evidence N min old") and in
`upgradescope_addon_evidence_age_seconds`. It is not part of the snapshot
hash, on the agent or on the server, so a reusing tick pushes nothing new
(`TestSnapshotHashIgnoresAddOnEvidenceAge`,
`TestAddOnEvidenceAgeIsNotANewSnapshot`), and no gap is raised for a reused
pass: it was complete.

**Why 3.** At the 2,001-node fill the reused ticks cost 20 requests and the
full ones 31, so a cycle of 3 averages 23.7 requests (computed), under the
25 of the issue, and 2 of every 3 ticks are under it by 5; 2 would average
25.5 (computed) and miss it, and each tick more is another tick of
staleness for a saving that is already there. The pod pages the
reused tick still makes are the 5 requests (17.4 MiB) of the `kube-system`
list: 4,000 pods at this fill (a CNI and a kube-proxy pod per node).

Run 1, `BENCH_TICKS=8` at the default cadence, was ticks 1, 4 and 7 full
passes (the first also filling the Helm cache) and 2, 3, 5, 6 and 8 reusing
the last. Medians of the steady ticks of each kind (the mean of the middle
two when the count is even; the heap is the maximum), `0b6fec28`, 10 October
2026, 02:56 to 03:12 UTC, the measurement lock held, lab B idle:

| Tick | Ticks | Requests | LIST pods | Response MiB | Wire MiB | Wall s | CPU s | Peak heap MiB | Peak RSS MiB |
|---|---|---|---|---|---|---|---|---|---|
| lists every pod | 2 (4, 7) | 31 | 16 | 50.4 | 3.3 | 4.4 | 3.4 | 30.5 | 63.6 |
| reuses the pass | 5 | 20 | 5 | 27.8 | 1.5 | 2.4 | 1.8 | 36.4 | 63.6 |
| the first tick | 1 | 1,037 | 16 | 72.7 | 24.2 | 35.1 | 23.3 | 36.3 | 56.7 |

The peak RSS is the process's peak so far, over every tick of the run (63.6
MiB at the end, against 56.7 at the first tick). The age of the evidence
was 3 to 7 seconds, because the benchmark runs its ticks back to back; in an
agent it is a multiple of the interval. The peak heap of a reusing tick is
the higher by about 6 MiB (32.6 to 36.4 MiB over its 5 ticks, 29.7 and 30.5
over the 2 full ones): the cache holds the evidence of the pass, but the
sampler reads every 5 ms with the garbage not yet collected, and these runs
did not separate the two. These figures come from `0b6fec28`, where a
full-pass tick still held the previous pass while it read the new one;
`81761afd` drops it first. The held pass is a few tens of KiB at this
fill (the estimate under [the short answers](#the-short-answers), checked
from the seed, not re-measured), so the peaks above are those of `daf513a4`
to within what the sampler can see. The requests of the two kinds of tick, by verb and
resource:

| Verb | Resource | Lists every pod: requests, MiB | Reuses the pass: requests, MiB |
|---|---|---|---|
| LIST | pods | 16, 40.09 | 5, 17.42 |
| LIST | nodes | 3, 5.37 | 3, 5.37 |
| LIST | secrets (metadata) | 3, 0.42 | 3, 0.42 |
| GET | `/metrics` | 1, 4.41 | 1, 4.40 |
| others (`/version`, `clusterreadinesses` GET and UPDATE, namespaces GET and LIST, ConfigMaps (metadata), CRDs, IngressClasses) | 1 each, 8 in all | 0.14 | 0.14 |

Run 2, on the same fill with `--pod-pass-every=1`: 31 requests on all 5
ticks, 50.5 MiB (46.0 on tick 3), 3.3 MiB on the wire (3.1 on tick 3), and
3.3 to 3.6 CPU-seconds on three of them (ticks 2, 3 and 5; 4.0 on tick 4). Its wall times are not usable:
the host's load average was 9 to 22, and tick 3 took 52 s because its
`/metrics` scrape ran into the 30 s request timeout (the benchmark flags
that tick: its `deprecated-calls` capability was unavailable, and run 2 exits
non-zero), tick 4 31 s. Run 2 confirms that the 31 requests of a pass tick are
the same with the setting off; the numbers of the table above are run 1's.

**The cheap early trigger was not built.** A metadata-only list of
Deployments, DaemonSets and StatefulSets with their resourceVersions
(or generations), compared with the last pass, could force a pass when a
workload changed. At this fill it is not cheap, computed from the object
counts and not measured, at the collector's metadata page size of 500
(`listPageSize`, PF-07: ceil(N / 500) requests): 4,000 Deployments are 8
pages, and the DaemonSets and the StatefulSets (fewer than 500 of each) 1
each, 10 requests more on a tick that makes 20 or 31 (the list is made on
every tick), which puts a reusing tick at 30, a tick that lists every pod
at 41 and the mean of a cycle of 3 at 33.7, over the issue's 25 by 8.7; resourceVersions of a Deployment change with every
status update, so a busy cluster would force a pass almost every tick; and
it does not see a pod that no such workload owns (a Job, a CronJob, a bare
pod or an operator's pod), so the staleness bound would still be the
count. Generations would see spec changes only, at the same request cost.
The staleness bound above is the same with it or without it, which is why
it is the only one documented.

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
  CPU-seconds, more than 132 releases at the mean cost explain (about 3 for
  them, 3 for the rest). It was not a decode again of what the partial tick
  had read, and the 132 were not average:
  [below](#the-tick-after-a-partial-helm-step-247), re-measured, the tick
  after a partial one fetched exactly the releases left unread, and what
  they cost to decode, the largest releases of the fill, explains its CPU. A steady tick took
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

#### The tick after a partial Helm step (#247)

[#247](https://github.com/abd-ulbasit/upgradescope/issues/247) asked whether
the releases a partial Helm step decoded are cache hits on the next tick.
They are: `TestHelmCacheKeepsWhatAPartialStepDecoded` stops a step at its
deadline part way through 40 releases against a fake API, and the next tick
fetches exactly the ones left unread. A hit makes no GET and no decode, so
a GET count pins it. Re-measured as a pod at 200m, with the procedure
above and `hack/bench/pod-sample.sh`, the tick after a partial one fetched
exactly the releases left unread, and still used 3 to 4 times the CPU of a
steady tick. **That CPU is the unread releases', and nothing else: they are
the largest releases of the fill, and the mean cost per release the first
estimate used does not describe them.**

**The runs.** The agent as a pod at 200m, the chart installed with
`agent.resources.limits.cpu=200m` and the default interval of 10 minutes,
on lab `us-lab-137b` reset and filled with `BENCH_STEPS=1` (2,001 nodes,
1,000 Helm releases, no Argo CD or Flux) just before, in the state given
in [What was measured](#what-was-measured-and-on-what). The CPU of a tick
is the counter's change between the samples around it (every 5 s, at most
11 s outside the tick, while the agent idles); the requests per tick are
from the lab's apiserver audit log, filtered to the agent's service account.
The first row is the earlier run (`da90a86e`, 9 October 2026, 17:46 to
18:12 UTC, the lab then at `86500c36`); the others are this branch's
(`9687cffc`, 9 October 2026), each a fresh pod.

| Run | First tick: wall s, CPU s, Helm step | Second tick: wall s, CPU s, release GETs | Steady tick: wall s, CPU s | Second less steady, CPU s |
|---|---|---|---|---|
| `da90a86e` | 75.5, 15.0, gave up with **43** of 1,001 unread | 51.9, 9.0, 43 | 29.2, 2.9 | 6.1 |
| `9687cffc`, profiler on | 67.8, 12.9, gave up with **51** unread (950 read) | 57.4, 10.0, 51 | 16.2, 2.5 | 7.5 |
| `9687cffc`, profiler off | 67.3, 13.1, gave up with **83** unread (918 read) | 68.5, 11.3, 83 | 17.8, 2.8 | 8.5 |

How many a first tick leaves unread is a race against its step deadline (the
Helm step gets at most 54 s of the 5-minute tick, the apiserver and the
host share the same four threads), so it differed in every run, 43 to 83.
The second tick made exactly that many Secret GETs each time (the lab's
audit log, ResponseComplete only: 43, 51 and 83 `get secrets` in the second
tick, after 965, 957 and 925 in the first) and the status the tick before
it wrote named them (`helm (partial)`, "83 release(s) not read, first
bench-ns-091/rel-0891"). Every other request of the second tick was a
steady tick's 31 (114 in all in the last run, 82 in the one before, 31 in
its steady tick). The quota throttled the first ticks in 96 to 98% of their
CFS periods, the second in 84 to 91% and the steady in 69 to 81%.

**Where the CPU goes.** With the profiler on (`--pprof-addr`, reached with
`kubectl port-forward`; [Profiling the agent](../observability.md#profiling-the-agent)),
CPU profiles of 30 s were taken from just before each later tick was due
until it ended (the first tick's was one of 90 s). Samples of the second tick and of the steady third tick of that
run, in CPU-seconds (9.64 and 2.66 sampled, against 10.0 and 2.5 in the
cgroup, so the profile's total is 96% and 106% of the cgroup's):

| | Second tick | Steady tick |
|---|---|---|
| `decodeHelmEntry`: gzip and JSON of the stored release, then its manifest parsed for the flagged APIs (4.49 and 1.15) | **5.64** | 0.00 |
| the 51 payload GETs, on the fetch workers | 0.31 | 0.00 |
| the garbage collector's mark workers | 1.16 | 0.34 |
| add-ons (the kube-system pods) | 1.22 | 1.23 |
| versions | 0.44 | 0.39 |
| deprecated-API callers | 0.41 | 0.40 |
| the status write and the evaluation | 0.12 | 0.12 |
| everything else | 0.34 | 0.18 |

Every function a steady tick runs cost what it cost in the second tick, to
within 0.05 CPU-seconds; what is added is the 51 releases: 5.64 to decode
them, 0.31 to fetch them, and 0.82 more garbage collection, which their
decoding allocates (the second tick's profile less the steady one's: 6.98,
of which 6.77 is these three, 97%). The first tick's profile (12.59
sampled) has the same shape: `decodeHelmEntry` 8.21 of it, for 950
releases.

**Why 51 releases cost that.** The estimate that "43 releases take about
0.8" divided the first tick's CPU by 1,000 (19.6 ms a release, outside a
quota, which includes the GET, the garbage and the rest of the tick), and
multiplied. It assumed the unread were average. They are not: the Helm step
visits releases by namespace and name, a step that gives up leaves the end
of that order unread, and the fill's large releases (300 objects, 20 bundled
files, 157 KB stored against 12 KB for a small one; 5% of the releases) are
exactly its last 50 ([What is simulated](#what-is-simulated)).
`TestHelmDecodeCostByRelease` (opt-in, `UPGRADESCOPE_HELM_PAYLOADS`: it
feeds the lab's own 1,000 payloads, read with `kubectl`, to `decodeHelmEntry`
one at a time and reads the process's CPU time) measured on the ThinkPad,
outside a quota, twice:

| | Run 1 | Run 2 |
|---|---|---|
| all 1,000 releases | 12.5 s (12.5 ms a release) | 12.3 s (12.3 ms) |
| share of the CPU, by twentieth of the order, the first sixteen | 2% each | 2% each |
| the seventeenth to nineteenth | 8% each | 9, 8 and 9% |
| the last twentieth (the 50 large releases) | 42% | 41% |
| the last 43 / 51 / 83 releases | 4.56 / 5.29 / 5.92 s | 4.37 / 5.06 / 5.73 s |
| the last 43 / 51 as a multiple of that many mean releases | 8.5x / 8.3x | 8.3x / 8.1x |

So the unread of the three runs cost 4.4 to 4.6 s, 5.1 to 5.3 s and 5.7 to
5.9 s to decode (outside a quota, with nothing else in the process), where
the mean said 0.5, 0.6 and 1.0. The second tick's CPU above a steady tick's
(6.1, 7.5 and 8.5 s) is 1.37, 1.45 and 1.46 times the decode alone (the
means of the two runs). The rest of that factor is what the profile found
above: the fetches and the garbage (6.77 against 5.64 s, 1.20 times), the
pod's decode taking 5.64 s where the harness took 5.1 to 5.3 s for the same
51 releases, and the cgroup's excess being 7% above the profile's (7.5 s
against 6.98). That is the acceptance: the CPU of the tick after a partial
step is explained by the unread releases alone, to 97% in the profile. The
other two runs were not profiled: their excess over the harness's decode
(1.37 and 1.46 times) is in line with the profiled run's 1.45, which is
consistent with the same account but does not prove it separately.

**What was not found, and what did not change.** Nothing in the second tick
is done again: no release is decoded twice, no request is repeated, the
discovery cache and the other caches answered as in a steady tick, and the
engine, the status write and the push cost what they cost then. So there is
no CPU to remove from that tick without making a release cheaper to decode,
which is another change (the manifest of a large release is parsed twice,
by the walk that locates each object and by kubectl's own decoder, which
the walk is checked against: 4.49 of the 5.64 s are the manifest, 2.73 of
them kubectl's decoder) and was not made here; it was made afterwards, for
[#285](https://github.com/abd-ulbasit/upgradescope/issues/285), and [is
measured below](#a-manifest-document-parsed-once-285). At
a mean cost the unread cost less than they did here; on a cluster whose
largest releases do not sort last, the tick after a partial step costs about
the unread count times the mean (12.4 ms, the mean of the two harness runs,
times 43 to 83: 0.5 to 1.0 s, computed). The first tick at 200m used 12.9
to 13.1 CPU-seconds in these two runs (15.0 at `da90a86e`), against 22.0 to
22.8 for the same first tick in the benchmark outside a quota; why a tick
costs less CPU under a quota was not investigated.

- At 200m the first tick still does not finish its Helm step, and leaves 43
  to 83 releases unread (132 at `735751d`). The chart's default is 1 CPU
  (below).
- A pod started minutes after a fill may be killed by its liveness probe
  while the lab's control plane is still busy (the dropped attempt above);
  wait until it is quiet, or the first tick is not a first tick.

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

#### A manifest document parsed once (#285)

[#285](https://github.com/abd-ulbasit/upgradescope/issues/285) found that a
release's stored manifest was parsed twice, once by the walk that locates
each object (yaml.v3, which gives every object its line) and again by
kubectl's own decoder (go-yaml v2, then JSON, then the unstructured scheme),
which the walk is checked against. The second parse is gone: a YAML document
is read once, by yaml.v3, and kubectl's decoder is given the JSON made from
the nodes that read produced (`treeJSONConverter`, `internal/collect/yamljson.go`).
The rest of kubectl's decoder, JSON to objects, still runs on every
document, so the cross-check stays what it was. It is not that the walk's
objects are reused: the walk and kubectl's decoder disagree on purpose
where YAML 1.1 and yaml.v3 do, and the check exists to catch that.

The JSON has to be the one kubectl's own decoder would make, so the
converter answers only for text where yaml.v3's tree cannot differ from
go-yaml v2's reading, and the document goes to kubectl's decoder, as before,
for anything else: an anchor, alias or tag, a merge key, a key that is not a
string, a duplicate key, a plain scalar that go-yaml v2 might read as a
number in a way only it knows (asked of kubectl's decoder itself, at most
256 distinct scalars a document), and text its line-by-line reader changes
(no final newline, a carriage return, a UTF-16 mark, a `!` that starts a
node). **How often it answers** depends on the manifest: all 33,620
documents of the lab's 1,000 releases, and all 58 documents of Flux's
install manifest (645 KiB with its CRDs, helm-controller v1.2.0), the
Kubernetes Dashboard's, this chart's render (with and without
`server.enabled`) and a cert-manager CRD
(`UPGRADESCOPE_PARSE_ONCE_FILES`, below). In an earlier run over two later
Flux install manifests, since deleted from the scratch directory, 2 of 81
documents went to kubectl's decoder: CRDs whose CEL rule continues on a line
that starts with `!=`, which reads like a tag. The first version of the
change declined 42% to 49% of the documents of three Flux install manifests
(the CRDs, which cost the most) on flags (`--events-addr=http://...`),
JSONPaths and `&& !has(...)` rules; it was changed before the timed runs
below.

**Same findings.** The HE-04 bounds are not touched (runs of at most 1 MiB
and 64Ki YAML nodes, a document over 2 MiB not parsed). The findings are
held to the same code run with kubectl's decoder reading the text, as it did
before (`parseManifestStreamWith`, `manifestAPIsWith` and `decodeHelmEntryWith`
with `reparse` set), by `TestParseOnce_CorpusStreamsMatchTwoParses` (every
manifest of the collector's test data, objects, evidence and problems),
`TestParseOnce_EveryHelmDocumentMatchesTwoParses` and
`TestParseOnce_ReleasesHaveTheSameFindings` (a table of releases: ingresses
and lists, a CRD, anchors, duplicate and merge keys, tags, JSON, comments,
unrendered templates), `TestParseOnce_RandomManifestsMatchTwoParses` (300
manifests of random documents, some past a run's size) and
`TestParseOnce_BoundsAreUnchanged`; and the JSON is held byte for byte to
kubectl's by `TestTreeJSON_MatchesKubectl` (every scalar YAML 1.1 has an
opinion on, as value, key, item and in flow collections),
`TestTreeJSON_Documents`, `TestTreeJSON_RandomDocuments` (20,000 documents)
and `FuzzTreeJSONMatchesKubectl` (the fuzz target ran 917,000 and 851,000
executions on the two versions of the converter without a difference; while
the converter was written it, and the random documents, found a bare `!`
tag, a text with no final newline, a UTF-16 mark and a line starting `---`,
each now a case of the tables and a reason in `readsTheSame`). With
`UPGRADESCOPE_HELM_PAYLOADS`, `TestParseOnce_PayloadsDecodeToTheSameEntries`
decodes the lab's 1,000 payloads both ways: the 1,000 entries are equal
(20 of them with flagged APIs). With `UPGRADESCOPE_PARSE_ONCE_FILES`,
`TestParseOnce_FilesMatchTwoParses` does the same for manifest files you
name and says how many of their documents the converter answered.

**Decode CPU.** `TestHelmDecodeCostByRelease` on the lab's 1,000 payloads,
before (`4b1c2893`) and after (`572f0e47`), four runs of each, alternating,
on the ThinkPad as above. Each cell is the mean of the four runs, with the
lowest and highest in brackets:

| | Before | After | Less |
|---|---|---|---|
| all 1,000 releases | 9.175 s (9.156 to 9.211) | 6.530 s (6.503 to 6.560) | 2.645 s, **28.8%** |
| the last 43 releases | 3.315 s (3.290 to 3.350) | 2.315 s (2.309 to 2.323) | 1.000 s, 30.2% |
| the last 50 (the large ones) | 3.849 s (3.813 to 3.892) | 2.694 s (2.675 to 2.712) | 1.155 s, 30.0% |
| the last 51 | 3.864 s (3.830 to 3.906) | 2.705 s (2.687 to 2.721) | 1.159 s, 30.0% |
| the last 83 | 4.359 s (4.334 to 4.399) | 3.062 s (3.034 to 3.091) | 1.298 s, 29.8% |
| share of the CPU in the last twentieth | 42% | 41 or 42% | |

The runs agree within 0.06 s of each other and the ranges of before and
after do not touch. The gain is the same in every part of the order, so the
large releases' share of the cost did not change (42%, then 41 or 42%): they
save what the others do, in proportion. **These absolute figures are not those of
[#247's two runs](#the-tick-after-a-partial-helm-step-247)** on the same
code (12.5 and 12.3 s for all 1,000, 4.37 and 4.56 s for the last 43,
at a host load of 4.6 and 7.2): this host was quieter (0.9 to 1.7 here), and
only a before and an after run back to back, as here, compare. The decode is
gzip and JSON of the release as well as its manifest, which this change does
not touch, so the manifest's own share fell by more than the 29%: in CPU
profiles on the Mac (a load average of about 50, so the
shares only), the manifest was 4.0 of 4.9 CPU-seconds of decoding before and
2.3 of 3.4 after, the yaml.v3 read of it 1.2 s in both, and the release's own
gzip and JSON 0.9 and 1.0 s (profiles of the test before and after the first
version of the change, three passes each).

On the manifests of real charts the gain is larger than the lab's, because
their CRDs, which the generated releases lack, are most of their text. The
Flux install manifest (645 KiB, 38 documents) takes 42, 43 and 46 ms of CPU
to read once and 68, 69 and 69 ms twice (`TestParseOnce_FilesMatchTwoParses`,
three runs of the test, each the mean of 3 readings, on the Mac at a load
average of 71 to 72: about 36% less, with that noise). No release of a real
cluster was timed.

**Heap.** The Helm heap tests (`UPGRADESCOPE_HEAP=1`, no `-race`, as
`make test-heap` runs them), before (main `4b1c2893`, test binary built from
it) and after, on the Mac at a load average of 31 to 68 for the pairs and
72 to 86 for `make test-heap` (so the readings are noisy: the harness reads
the live heap after back-to-back collections, and a busy host adds to it).
The bound is 64 MiB. Peak live heap above the baseline, MiB, per case of
`TestCollectHelmManifestParsingIsBounded`. "First version" is `db5b0845`,
whose two runs alternated with the two before runs; "final" is `572f0e47`,
one run, from `make test-heap` (10 October 2026):

| Case | Before, run 1 | Before, run 2 | First version, run 1 | First version, run 2 | Final |
|---|---|---|---|---|---|
| newlines | 16.1 | 24.1 | 24.1 | 32.1 | 16.1 |
| tiny flagged objects | 30.8 | 18.2 | 32.1 | 32.1 | 32.1 |
| documents that are not objects | 34.3 | 44.1 | 16.1 | 32.1 | 44.1 |
| objects of newlines at the size bound | 24.1 | 24.1 | 32.1 | 16.1 | 40.1 |
| objects at the node bound | 38.9 | 29.7 | 31.5 | 19.4 | 20.5 |

Before and after read in the same range (16.1 to 44.1 MiB both), and none
comes near the 64 MiB bound: no change in the heap is measured, in either
direction, and these tests could not resolve a small one. What the code
gives is that the nodes the walk built are the ones the JSON is made from,
and the maps built from them replace the ones go-yaml v2 built, so no more
is held at once than before.
`TestCollectHelmPeakHeapIsBoundedByOneRelease` read 18.0 to 20.0 MiB before
and 16.2 to 21.9 after (20.0 and 16.9 in the final run);
`TestCollectHelmGzipBombIsBounded` read the same in every run (16.7, 0.7,
0.7, 0.1 and 16.1 MiB). The CI runner's figures for HE-04 (32 to 46 MiB)
are from before this change and were not re-measured there.

**What is not claimed.** The CPU of a pod's tick after a partial Helm step
was not re-measured: no pod ran this code. Its 5.64 CPU-seconds of decoding
for 51 releases ([above](#the-tick-after-a-partial-helm-step-247)) would be
about 30% less if it follows the harness (about 4.0, computed). A first tick
at 200m would most likely still give up its Helm step (computed: the 19 to
20.5 CPU-seconds [above](#cpu-and-the-chart-limit) that decoding the 1,000
takes, less 29%, is 13.5 to 14.6, against the 11.8 that 200m gives the
step's 59 s). A document that goes to kubectl's decoder costs what it
did, and a little more for the attempt. The JSON the converter makes is read
again by the unstructured scheme (0.5 of the 3.4 CPU-seconds in the profile
above); skipping that would copy the walk's list and item rules a third
time, for the gain of a sixth of the decode, and was not done.

### Hotspots

| What | Found | Status |
|---|---|---|
| One GET per Helm release on every tick: 1,000 of 1,061 requests, 90 MiB, 33 s and 23.5 CPU-seconds at full size | The only cost that grew with releases, not pages | **Fixed**: the agent keeps what it decoded, keyed by the storage object's UID and resourceVersion; a steady tick makes none (61 requests then, 53 on `735751d` and 31 after #228). `TestHelmCache…` and `TestTicksFetchHelmReleasesOnlyWhenTheyChange` |
| The same 1,000 GETs on the first tick after a start, and on every one-shot `scan` | Sequential, 36.1 to 36.7 s and 23.1 to 23.5 CPU-seconds here; at a 60 ms round trip the first tick reached the Helm step's deadline with 231 releases unread; as a pod at the old 200m default it reached it with 132 unread (measured, [above](#cpu-and-the-chart-limit)) | **Fixed** for the round trip ([#226](https://github.com/abd-ulbasit/upgradescope/issues/226)): 8 GETs in flight, decoded one at a time in order; 42.4 s here at `9810fb6`, on a host whose load rose to 15 (27.1 s at the draft `59d8561`, at a load of 7 to 12), and at 60 ms 30.4 s at `06cdf7a` and 29.6 s at `5da764e`, with every release read. Decoding (22 CPU-seconds) and the client's rate limit (14 s for 1,000) are the bounds now, so under a CPU quota #226 was not expected to help at 200m and saves at most the 13.4 s the GETs waited at 1 CPU (computed, [above](#cpu-and-the-chart-limit)); measured at 200m at `da90a86e` (with #226 and #228, 9 October 2026), the first tick still gave up at the Helm step's deadline, with 43 of 1,001 unread where `735751d` left 132 ([above](#the-tick-after-a-partial-helm-step-247)); the runs differ in more than #226 and in host load, so the share that is #226's is not known; the chart's default limit is now 1 CPU, at which main's first tick read all 1,000. `TestCollectHelmConcurrentMatchesSequential` |
| A Helm release's manifest is parsed twice per YAML document: by the walk and by kubectl's decoder ([#285](https://github.com/abd-ulbasit/upgradescope/issues/285)) | 4.49 of the 5.64 CPU-seconds of decoding 51 releases in the pod, 2.73 of them kubectl's decoder ([above](#the-tick-after-a-partial-helm-step-247)); the lab's 1,000 releases cost 9.175 s of CPU to decode (mean of four runs, the ThinkPad, `4b1c2893`) | **Fixed**: the second parse is gone where it can be shown to give the same JSON: 6.530 s (28.8% less) for the 1,000, 30% less for the large ones, `572f0e47`, 10 October 2026; 36% less to read Flux's real install manifests (Mac, noisy); same findings and heap ([above](#a-manifest-document-parsed-once-285)). `TestParseOnce…`, `TestTreeJSON…` |
| `kube-system` pods are listed in full twice a tick: by the control-plane version check, then again by the all-pods list | 4,009 pods here; the second listing is not a separate 9 requests but 9 extra ones (those pods already fall inside the 29 pages of the all-pods list) and about 4,000 pods decoded twice; grows with nodes (every node adds a `kube-proxy` and a CNI pod) | **Fixed** ([#227](https://github.com/abd-ulbasit/upgradescope/issues/227), #232): the add-ons take them from the version check's list; 30 pod list requests a tick instead of 38 in the runs on `735751d` |
| Argo CD Applications, Flux HelmReleases and the OCIRepositories their chartRefs name are listed whole (page size 50) every tick (#218; the OCIRepositories since #248, a GET each before) | **Measured** (`BENCH_GITOPS=1`, at 1,000 Applications and 1,000 HelmReleases with 500 chartRefs): at `da90a86e` 50 of the 81 requests of a steady tick (20 + 20 + 10 lists), 9.0 MiB of 59.8, every chart resolved ([above](#the-ocirepositories-listed-248)); on main `735751d`, before #248, 540 of 596, 500 of them sequential OCIRepository GETs, 12 MiB of 63, 8.7 more seconds and 3.2 more CPU-seconds, under host noise ([above](#re-measured-on-main-with-and-without-argo-cd-and-flux)) | Fixed by [#248](https://github.com/abd-ulbasit/upgradescope/issues/248): a list instead of a GET per OCIRepository, a GET by name only where the list is forbidden; the rest grows by one request per 50 objects |
| The all-pods list is the steady tick's largest cost | 38 of 61 requests (30 of 53 after #232), 79% of the bytes; whole pod objects are needed for their images, and the agent keeps no watch | **Requests fixed** ([#228](https://github.com/abd-ulbasit/upgradescope/issues/228)): pod and node pages sized by their largest object (at most 1,000), discovery kept between ticks, the agent's object read once: 53 to 31 requests, short of the target of under 25. The bytes (50 MiB) and CPU (3 s) are unchanged on a tick that lists every pod; reading the pods less often (#228's option b, the default since, PF-18) makes the ticks that reuse the pass cost 20 requests, 27.8 MiB and 1.8 CPU-seconds (measured at `0b6fec28`, 10 October 2026), and a cycle of 3 averages 23.7 requests (computed), while the tick that lists every pod still makes 31 |

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

# The pod pass reuse (#228): the default cadence needs ticks of both kinds
# after the first, so run enough (BENCH_TICKS=8 gives 2 that list every pod
# and 5 that reuse the pass); BENCH_POD_PASS_EVERY=1 lists every pod every
# tick, as before. The report tells the two kinds of tick apart.
BENCH_STEPS=1 BENCH_TICKS=8 BENCH_RUN_ON=lab-host make bench-agent KUBECONFIG=/path/to/lab-kubeconfig

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
`BENCH_HELM_REVISIONS`, `BENCH_RESET_CMD`, `BENCH_NO_HELM_CACHE`, `BENCH_POD_PASS_EVERY`, `BENCH_GITOPS`); both scripts print their tables
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
- The pod pass reuse (#228) was run in the benchmark process only, not as a
  pod, and without Argo CD or Flux objects, on one fill whose pods never
  changed: its staleness rules are shown by tests against a fake apiserver
  (`TestPodPassStalenessIsBoundedByTheCount` and the others of PF-18), not
  by a lab in which an add-on was upgraded. The age of the evidence in the
  runs, 3 to 7 seconds, is the benchmark's back-to-back ticks, not an
  agent's. The lab's pods run near-identical images, so the cache was small;
  a cluster with many distinct images keeps more (at most what one full
  pass holds while it matches, never the pods), which was not measured. The
  wall times are a loaded host's.
- The server benchmark generates inventories: real ones differ in the size
  of their Helm releases and the findings they carry. The per-snapshot
  storage scales with the inventory.
- The 60 ms round trip is a delay netem adds to what lab A's control plane
  sends: latency only, no bandwidth limit and no loss. The fake apiserver of
  `TestCollectHelmColdFetchAtRoundTrip` adds it as a sleep before each
  answer, and serves generated releases, so the decoding there is a laptop's
  and not the lab's.
