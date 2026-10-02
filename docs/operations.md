# Running the server

What it takes to run `upgradescope serve` as a fleet hub for months: how a
push is judged, a cluster lifecycle, stale-data signals, notifications a
receiver can trust and auditor exports. The Helm chart's fleet-hub install
is in the [chart README](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/chart/README.md#fleet-hub-server-only).
Elsewhere:

- storage, pruning and backups: [Retention and backup](operations/retention-and-backup.md);
- tokens, SSO and who can read what: [Tenancy and access control](operations/tenancy.md);
- metrics, probes and logs: [Metrics, logs and probes](observability.md);
- every endpoint: the [REST API reference](reference/api.md).

## Memory and request limits

The server bounds the memory each request can make it use, rather than
hoping requests are small: by its size and structure, by how many run
at once, and by how much of their answers may wait for slow clients.
Those bounds do not cover everything: what is outside them, with what it
costs, is listed after the worst case below. Decoding is what costs memory, and it costs per node, not
per byte: a 4 MiB YAML flow sequence `[1,1,…]` decoded to ~900 MB of
heap, and 20 MiB of `{}` object refs in a snapshot to ~2.6 GB. So each
request is measured before it is decoded:

| | `POST /api/v1/gate` | `POST /api/v1/snapshots` |
|---|---|---|
| body cap (wire, and decompressed) | `--max-gate-bytes`, 10 MiB | `--max-snapshot-bytes`, 20 MiB |
| with aliases expanded | the whole stream within the body cap, each document within 4 MiB; 20,000 documents | — |
| node budget, counted from the raw bytes | 400k units: a YAML node 1, a sequence entry 4, an alias what it names | 1M units: a JSON value 1, an object 8 |
| answer | at most `--max-gate-bytes`, bounded before it is encoded; `?path=` at most 512 bytes | — |
| worst live heap within the budget (measured on SQLite) | ~165 MiB, with `?cluster=` too, the answer included | ~120 MiB, the body's copy and the reports it stores included |
| bodies buffered across requests | 3 × the cap (30 MiB) | 2 × the cap (40 MiB) |
| measured for aliases, decoded and evaluated at once | 1, others wait up to 30s holding only their bodies | 1, others wait up to 10s |

Over a cap or a budget on the input is `413`, before the body is
decoded (an answer over its bound, below, after the evaluation); the
message says to split the stream or List. A body that does not fit the shared
buffer budget, or a request that waits too long for its turn, gets `503`
with `Retry-After` (the agent retries it). A body must arrive within the
60s read timeout (about 350 KiB/s at 20 MiB), or it gets `408`, which the
agent retries too. The node counts come from an emulation of the YAML
scanner and the JSON tokenizer, fuzzed never to count fewer nodes than
yaml.v3, kubectl's decoder and `encoding/json` build. YAML aliases are
charged what they name: kubectl's decoder copies the aliased node at
every alias (merge keys included), and go-yaml v2's excessive-aliasing
check neither starts before 100 aliases nor counts a scalar's bytes, so
one 3.5 MiB anchored string and 99 aliases of it decoded to ~2 GB.
What the aliases add counts against the document's 4 MiB and against
the body cap for the whole stream: 136 documents of 40 KB, each within
its 4 MiB, named 100 objects each after one anchored 40 KB string, and
that 5.8 MB stream answered 532 MB and took 1.8 GiB of heap. A
document in which the meter finds an alias (a `*` token: not one in a
string, a comment or a glob such as `get*`) is read into yaml.v3 nodes
before it is decoded, and what its aliases expand to is added to the
node count and to the document's size. That read costs about what
decoding does (no more: the node count is checked first), so it waits
for the evaluation slot too; measured before it, 13 requests of 2.3 MB
at the node budget, all the body budget holds, took ~2.2 GB. A stream with
a UTF-16 byte order mark is `422`: the decoders read it as UTF-16, the
meter as UTF-8. A realistic ~4 MiB `kubectl get -o yaml` List of
Deployments is ~360k units and fits; typical kubectl YAML is ~90k units
per MiB, so the node budget, not the 10 MiB body cap, is what limits a
realistic stream, at about 4.4 MiB.

The answer is bounded too. Its size follows the strings the stream (and,
with `?cluster=`, the cluster) put in it, times how the format escapes
and repeats them: every listed object carries its name, its namespace
and `?path=`, SARIF and Code Quality write a result per object with its
finding's title and fix, and the encoders hold several copies of what
they write. Unbounded, a 1.2 MB stream naming 100 objects of each of the
KB's 136 deprecated or removed GVKs, with a 60 KB `?path=`, answered
817 MB and took a fresh server to 2.4 GB; with 650-byte names of `<` and
a 512-byte path, 9 MB answered 72 MB of SARIF and took 347 MiB of heap.
So `?path=` is at most 512 bytes (`422`), and after the evaluation, and
before anything is encoded, the server bounds the answer in the asked
format (every string at its dearest escape, each value with room for its
punctuation and indentation, what each format repeats), and an answer
whose bound is over `--max-gate-bytes` is `413`, saying to split the
stream. With `?cluster=`, the cluster's own findings and the objects
they list count too, so a cluster whose report lists thousands of
objects with long names can make its `?cluster=` answers `413` (raise
`--max-gate-bytes` for it). `TestGateAnswerBoundHolds` checks the bound
against every format, with and without `?path=` and `?cluster=`, for
every character class an escape lengthens; the largest answers within
it (up to ~7 MB) take at most ~33 MiB of heap to build
(`TestGateAnswerHeapIsBounded`).

Reads cost what was stored, so they are bounded too. A read of one
cluster (its detail, report, findings, teams, history or export) and the
fleet teams rollup load the cluster's latest snapshot, up to
`--max-snapshot-bytes` as pushed, which the SQLite driver holds twice;
a report for a target with no stored evaluation (a what-if) also decodes
and evaluates the whole inventory, at a cost per node like ingest's. One
370 KB push of `{}` object refs, accepted from any ingest token, took
~45 MB of heap for each such read, and 10 concurrent reads of it grew
the heap ~400 MiB. These reads run one at a time in a read slot of their
own, so they never wait on pushes or `/gate`; others wait up to 30s, then
get `503` with `Retry-After`. Only a what-if decodes the whole inventory;
the other reads decode its server version and capabilities and skip the
rest. One read costs what it loads: on SQLite, whose driver holds a copy
of every snapshot and report it reads beside the one it returns, a
report or its findings from a stored evaluation of a 17 MB snapshot (at
the node budget, every API-usage entry a finding, so the 17.5 MB report
is as large) grew the heap ~90 MiB, a what-if of it ~88 MiB, and the
same with object names of U+2028 up to ~97 MiB. Since they run one at a
time, 10 concurrent requests to any of these endpoints whose clients
take their responses at once add only the garbage of the read before:
up to ~97 MiB in all (`TestReadHeapIsBounded`, on SQLite at the
snapshot node budget, fails above 128 MiB).

A snapshot's strings are stored and served no longer than they were
pushed. `encoding/json` writes `<`, `>` and `&` as six-byte escapes
(and U+2028 and U+2029 always), so the same 17 MB push with object
names of `<` stored three 80 MB reports, grew the ingest heap 323 MiB
and the report read 170 MiB. The server writes its stored reports and
every JSON response without those escapes (the responses are
`application/json` with `nosniff`), hashes a push for deduplication
without holding the escaped copy (the hash is unchanged), and refuses a
push that is not valid UTF-8 with `422` (`encoding/json` would decode
each invalid byte to three). The heap tests store snapshots of `x`, `<`
and U+2028 names alike, and their figures here are the dearest of them.

A response is written to memory in the slot and sent after it, so a
client that is slow to read holds its response, not the slot. `/gate`
does the same with its answer: it is encoded in the evaluation slot,
after which the reports it was built from are garbage. So do the reads
of the whole fleet (below). These responses, the reads', the fleet
reads' and `/gate`'s, share one budget of twice
`--max-snapshot-bytes` (40 MiB) while their clients read them, for up to
the 120s write timeout. One that does not fit what is left of the budget
gets `503` with `Retry-After` instead (its work is lost, but no slot
waits on a client); one larger than the whole budget, which could never
fit, is sent in its slot, and a client that has not taken it within 20s
is cut off. Every such response carries its `Content-Length`, so a
client can tell a cut-off body from a whole one. Before the budget,
nothing capped how many responses were held: 20 clients that asked for
that 17.5 MB report and never read it grew the live heap 366 MiB with
the slot free, 10 that sent `/gate?cluster=` against that cluster and
never read the answer grew it 320 MiB (~32 MiB each: the report, the
answer and its encoding), and a 120s window holds about 100 of either.
Now 8 or 20 such readers over real sockets leave 33 MiB live (two
responses held, the rest answered `503`) and the heap peaks 124 MiB
above idle with the request in its slot, and 10 `/gate?cluster=`
clients whose 4.5 MB answers are among the largest the answer bound
lets through leave 39 MiB live, at a 61 MiB peak
(`TestUnreadResponsesAreBounded` and `TestUnreadGateResponsesAreBounded`,
which fail above the budget for what stays live, and at the peak above
168 MiB for reads and 240 MiB for `/gate`). A `/gate` answer is at most
`--max-gate-bytes`, under the budget, so it is never sent in its slot
unless `--max-gate-bytes` is set over twice `--max-snapshot-bytes`.

The reads of the whole fleet, `/clusters`, `/fleet` and `/metrics`, cost
about their response whatever was stored: they read each cluster's
server version from one query over the snapshot heads, and each
evaluation's score, verdict, counts and what it could not assess from
its own columns, never the stored report. Before that, one 17 MB push
made 30 concurrent requests to any of them grow the heap by 285-584 MiB;
now by at most 2 MiB (`TestFleetReadsLoadNoReport`). Their responses do
grow with the fleet: at 500 clusters `/clusters` is ~230 KB, `/fleet`
~480 KB (~590 KB with 16 `?targets=`) and `/metrics` ~740 KB, and
building one adds up to ~5 MiB to the heap (`/metrics` the most, about
five times its response); at 2000 clusters with 200-byte names, with two
`--targets`, they are 1.3, 2.3 (2.7 with 16 `?targets=`) and 9 MB, and
`/metrics` adds ~47 MiB. `/fleet?targets=` takes at most 16 distinct
minors (`422` above): each is a column and a store query per cluster,
and unbounded but for the 64 KiB URL, 8,718 of them against 500 clusters
held a fleet slot for 2m13s, grew the heap 418 MiB and answered 57 MiB.
They run two at a time in fleet slots of their own (the dashboard's
reads wait up to 30s, a `/metrics` scrape up to 5s, within Prometheus'
default 10s scrape timeout, then get `503` with `Retry-After`), and
their responses wait for their clients in the budget above. Written
straight to their clients, with nothing capping how many, 100 clients
that never read held 201 MiB of `/clusters` and 251 MiB of `/fleet` of
that 2000-cluster fleet, and 30 held 433 MiB of `/metrics`, whose handler
keeps the gathered metric families until its write returns; now 100 of
any of them, a 16-target `/fleet` included, leave at most the 40 MiB
budget live, and the heap peaks at most 168 MiB above idle with the two
builds in their slots (`TestUnreadFleetResponsesAreBounded`). A
Prometheus scrape or a dashboard poll that gets `503` is retried at its
next interval.

Worst case for the chart's 768Mi server, each part measured on SQLite
against the dearest snapshot at its node budget, names of `x`, `<` and
U+2028 alike (`TestGateDecodeHeapIsBounded`,
`TestStoredSnapshotHeapIsBounded`, `TestReadHeapIsBounded`,
`TestGateAnswerHeapIsBounded`, `TestUnreadResponsesAreBounded`,
`TestUnreadGateResponsesAreBounded`,
`TestUnreadFleetResponsesAreBounded`):
one `/gate` request in the evaluation slot (~165 MiB, with `?cluster=`
too, since the cluster's inventory is decoded once the manifests' node
trees are garbage, its answer included: answers big enough to cost more
to encode are cheap to decode) plus one ingest (~120 MiB for the 17 MB
push whose three stored reports are each as large, its copy of the body
included) plus one read in the read slot (~97 MiB, its response
included) plus two reads of the whole fleet in their slots (up to
~10 MiB for 500 clusters) plus the read, fleet read and `/gate`
responses held for their clients (the one 40 MiB budget) plus the
background re-evaluation pass, which takes clusters one at a time
(~92 MiB for that snapshot and three targets) plus both body budgets
(70 MiB; an ingest gives its share back once it holds that copy, so
another push can wait in it): about 595 MiB for a 500-cluster fleet,
inside the 691 MiB `GOMEMLIMIT` the chart derives from the limit. Below
about 665Mi, that sum no longer fits under `GOMEMLIMIT`. (Each figure is
a peak with its garbage, measured with the collector held near the live
heap; runs differ by a few MiB.)

What is outside these bounds, and what it costs:

- **Connections.** Nothing in the server caps how many connections a
  client opens. Each costs ~10 KiB of heap and an 8 KiB goroutine stack
  while it is open (measured with 1,000 clients that sent a request and
  did not read the answer), more while its request headers, up to
  64 KiB, are read: 10,000 open connections hold ~180 MiB. An idle
  connection is closed after 120s, one whose headers have not arrived
  after 10s, one whose body has not after 60s, and one whose response
  has not been taken after the 120s write timeout (20s for a response
  sent in its slot); a client can open new ones as fast as old ones
  close.
- **Socket buffers.** These are Go heap figures, and the kernel's socket
  buffers are not in them: Linux grows each connection's send buffer up
  to `net.ipv4.tcp_wmem`'s maximum (4 MiB by default), and on cgroup v2
  that memory is charged to the pod's limit, outside `GOMEMLIMIT`, so
  many connections that do not read can hold up to that much each, until
  a write deadline or the write timeout closes them and the kernel gives
  up on what they left unsent. The tests above shrink the server's
  buffers to 4 KiB, so they measure the heap alone.
- **The fleet's size.** Nothing caps how many clusters the server
  holds, and a holder of the shared ingest token registers a new one
  with each new name it pushes. What a fleet read costs grows with the
  fleet (above): about 5 MiB for 500 clusters, ~47 MiB for a `/metrics`
  of 2000 clusters with 200-byte names, twice that with both fleet slots
  busy. Without `?targets=`, `/fleet` has a column for every minor some
  cluster runs the next of, so clusters pushed at many minors widen it
  too.
- **Snapshots stored by v0.1.** A snapshot a v0.1 server stored before
  the budgets existed (up to 20 MiB of any shape) is decoded without a
  node count when `/gate?cluster=`, the re-evaluation pass or a what-if
  read reads it (a report, its findings or teams for a target with no
  stored evaluation, or the fleet teams rollup), until that cluster's
  agent pushes again. Those decodes take their slots, but one 20 MiB
  snapshot of `{}` object refs decodes to ~2.6 GB. Such a row also has
  no stored server version, so `/clusters`, `/fleet` and `/metrics` load
  it, up to 20 MiB, in their fleet slots, to read its version.
  (Evaluations stored before the `not_assessed` column are backfilled
  from their reports when the database is migrated, so no report is
  read for them.)

The held-response budget bounds memory, not who gets it. Any answer
larger than what is left of it gets `503`, so clients that ask for large
answers (a report, a `/gate` answer of up to `--max-gate-bytes`, or a
fleet read of a large fleet) and do not read them can keep the budget
full for up to the 120s write timeout, and ask again: meanwhile per-cluster reads, fleet reads
(a Prometheus scrape and the dashboard's polls included) and `/gate`
answers that do not fit get `503`. When the read API is open, that
needs no credentials.

The Go runtime does not read the container's limit, and without a
memory limit the collector lets garbage grow to as much as the live heap
again before it runs, so a process whose live heap fits is OOM-killed
anyway. Outside the
chart (`docker run -m`, systemd, any cgroup), `serve` and `agent` set it
themselves to 90% of the cgroup's memory limit (v2 `memory.max` or v1
`memory.limit_in_bytes`) and log it; an explicit `GOMEMLIMIT` wins.

Peak RSS of a fresh `serve` process (macOS arm64, `/usr/bin/time -l`,
SQLite, October 2026), `before` being v0.1.x without these limits:

| request | before | now |
|---|---|---|
| 4 MiB YAML flow sequence `[1,1,…]`, ×1 / ×2 at once | 913 / 1074 MB, 200 | 45 / 49 MB, 413 |
| 4 MiB List of null items | 2077 MB, 200 | 44 MB, 413 |
| 4 MiB of short keys (block sequence alike) | 412 MB, 200 | 44 MB, 413 |
| short keys at the node budget (2.3 MB), ×6 at once | 331 MB, 200 | 335 MB, 200 (298 MB with `GOMEMLIMIT=460MiB`) |
| the same with a `# *` comment / with one alias, ×13 at once (all the body budget holds) | — | 368 / 349 MB, 200 |
| realistic 4 MiB List of 1,400 Deployments, ×6 at once | 253 MB, 200 | 279 MB, 200 (249 MB with `GOMEMLIMIT`) |
| one 4 MiB string | 97 MB, 200 | 94 MB, 200 |
| 3.5 MiB string anchored, 99 aliases of it (3.6 MB) | 2024 MB, 200 | 65 MB, 413 |
| 4 MiB string aliased 729 times through three levels of 9 aliases | 3982 MB, 200 | 69 MB, 413 |
| 4 MiB UTF-16 flow sequence | 420 MB, 200 | 45 MB, 422 |
| aliases at the budget (42 KB string ×100; 5 KB ×729; 90 merges of a 22 KB mapping), ×6 at once | — | 84 / 84 / 119 MB, 200 |
| 20 MiB push of `{}` object refs | 3578 MB, 202 | 41 MB, 413 |
| 4.9 MB push of unique map keys (at the budget) | 184 MB, 202 | 190 MB, 202 |
| 30 concurrent 20 MiB pushes | 1681 MB, all accepted | 236 MB, 2 accepted, the rest 503 |

What is left is availability: a client that really sends 3 × the gate
cap and then stalls makes other `/gate` requests `503` until the read
timeout cuts it off (about 0.5 MiB/s of its bandwidth for 60s), and with
an open read API that needs no credentials. Likewise, a client that keeps
asking for what-if reports of a cluster at its node budget keeps other
per-cluster reads waiting, and some of them get `503`. A read token
limits both to token holders; the same holds for pushes and the ingest
tokens.

## What a push is judged as

- **Stored as sent.** A snapshot keeps the inventory bytes the agent pushed,
  `collectedAt` and fields this server does not know included, so a newer
  server can judge them. Deduplication hashes the fields this server knows,
  without `collectedAt`: a push that differs only in unknown fields is a
  duplicate (`200`), and the duplicate's `agentVersion` and `kbVersion` are
  recorded on the latest snapshot.
- **Refused before anything is written** (`422`): a missing or `null`
  inventory, an inventory `schemaVersion` other than 1 (including `{}`), or
  a `serverVersion` that is not a Kubernetes 1.x version.
- **Degraded pushes** (no `serverVersion`: the agent could not read
  `/version`) are judged at the version the cluster last reported, so its
  fleet cells, default target and report stay. The versions capability is
  required, so such a cell is `unknown` at best (`blocked` when the push
  shows a blocker), and the report's `notAssessed` says why. A cluster's
  very first push without a version is judged only at `--targets`.
- **v0.1.x agents** (`agentVersion` 0.1.0, 0.1.1 or earlier, their
  pre-releases and Go pseudo-versions) collected two signals with meanings
  this server no longer judges: api-usage counted every object the
  apiserver *serves* at a deprecated version (APF FlowSchemas became
  removed-API blockers) and their own requests landed in the
  deprecated-calls metric; a Helm-chart-found add-on's version was the
  chart version. Their api-usage and deprecated-calls are reported as not
  assessed, with the reason, and a chart version is kept as evidence only,
  so such a cluster is `unknown` until its agent is upgraded. Builds from
  later source (`dev`, a 0.1.2 pseudo-version or snapshot) are judged
  normally.
- **Outdated verdicts.** A stored verdict depends on the date (EOL windows),
  the KB and the team map. The background pass re-evaluates hourly and just
  after each UTC midnight; until it has, every read of a stored verdict
  (fleet cells, cluster summaries, the report) carries `"outdated": true`
  and starts the next pass early. Reads never recompute.

## Cluster lifecycle

A cluster is registered by its first push, under the agent's
`--cluster-name`, and bound to the cluster UID (the `kube-system`
namespace UID) it pushed. A push under that name from another UID, or with
no UID at all, is refused with 409, so two clusters never interleave in one
history.

Deleting and renaming need the admin token: `serve --admin-token`
(`$UPGRADESCOPE_ADMIN_TOKEN`, `--admin-token-file`; chart
`server.adminToken` or `adminTokenFromSecret`). Without one the server
refuses both (403); the read and ingest tokens get 403 too. The admin token
must differ from both, and it also authorizes reads.

| | API | CLI |
|---|---|---|
| list | `GET /api/v1/clusters` (read token) | `upgradescope clusters list --server URL` |
| delete | `DELETE /api/v1/clusters/{id}` → 204 | `upgradescope clusters delete <name> --server URL` |
| rename | `PATCH /api/v1/clusters/{id}` `{"name": "new"}` → 200 | `upgradescope clusters rename <name> <new-name> --server URL` |

The CLI takes the token from `--admin-token` (`--read-token` for `list`),
the environment variable or a `-file` flag. Without `--server` it works on
the database directly (`--db` or `--db-url`), for example while the server
is stopped.

- **Delete** removes the cluster, its snapshots and evaluations, its queued
  notifications, and the per-cluster ingest tokens minted for its name.
  An agent still pushing under the name registers it again, so stop or
  re-point the agent first. To re-register a rebuilt cluster (new UID,
  same name), delete the old record and mint a new token.
- **Rename** moves the history and re-binds the name's ingest tokens. The
  agent sends its own `--cluster-name` with every push, so change that too
  (chart `agent.clusterName`): until then its pushes are refused when it
  uses a per-cluster token, or register the old name again when it uses
  the shared ingest token.

Every delete and rename is logged by the server.

## Stale clusters

A cluster is **stale** when its agent has not pushed, duplicates included,
within `serve --stale-after` (default `2h`; chart `server.staleAfter`). An
unchanged cluster pushes on its agent's next tick after the hourly
force-sync, about every 70 minutes with the defaults, so a window under
about 80 minutes would flag healthy clusters. A stale cluster's scores
describe it as it was at `lastSeen`.

- `GET /api/v1/clusters`, `GET /api/v1/clusters/{id}` and every row of
  `GET /api/v1/fleet` carry `lastSeen` and `stale`.
- `/metrics` exports `upgradescope_cluster_stale{cluster}` (0 or 1) next to
  `upgradescope_cluster_last_push_age_seconds{cluster}`; the chart's
  `UpgradescopeClusterStale` alert fires on the push age.

## Notifications

With `--slack-webhook` or `--webhook`, the server sends a notification when
an evaluation pass changes a cluster's readiness: a new blocker, all
blockers resolved (became ready), or an add-on entering its end-of-life
window. Each pass is compared with the target's last evaluation that had
a decided verdict (ready or blocked). A pass whose verdict is unknown sends
nothing and is not a baseline either: what it could not see is not news.

A new cluster's first evaluation of a target is the baseline and sends
nothing, and so does the first evaluation of a target added to
`--targets` later, so that restarting the server with a new target does
not notify every cluster at once.

**After a cluster upgrade.** When a cluster upgrades, its default target
moves up one minor: a cluster on 1.35 is judged against 1.36; after its
upgrade to 1.36, against 1.37. The new default target's first decided
evaluation is compared with the previous default target's last decided one
(the nearest lower target that has one, up to 3 minors below). So:

- a blocker that the new target adds is a `new-blocker`. For example, a
  cluster that still calls `networking.k8s.io/v1beta1` ServiceCIDR, removed
  in 1.37, is notified when it reaches 1.36;
- a blocker the cluster already had at the old target (an EOL add-on, an
  API removed long ago) is not announced again. There is no separate
  "target changed" event;
- if the old target was blocked and the new one is ready, the cluster gets
  `became-ready`.

The same comparison applies when a newer server or knowledge base first
decides a default target that it could not judge before (a cluster on the
newest minor, whose next minor was past the knowledge base's horizon and
so `unknown`). Every cluster on that minor then gets its first decided
evaluation in the same pass. Each one that has a stored decided
evaluation of a lower target (within three minors below) is compared with
it and notified once of the blockers that are new to it (at most one
notification per cluster, its changes capped as usual). A cluster without
one, because it was first seen on the newest minor or because its
lower-target evaluations were pruned, has nothing to compare with, and
its first decided evaluation is a silent baseline.

This differs from a target added to `--targets`, which stays silent: adding
a target asks a new question about a cluster that has not changed, and
every blocker it finds was already there. A default target that was
unknown until a knowledge-base update is the cluster's real next upgrade,
and the update is the first time anyone could say what blocks it, so those
blockers are genuinely new to each cluster and are announced, once.

Retention edge: the old target's evaluation is the baseline only while it
is stored. If a new default target stays `unknown` for longer than
`--retention` after the upgrade (no knowledge base for it that long), the
old target's evaluations are pruned (see [Retention and
backup](operations/retention-and-backup.md)), and the first decided
evaluation of the new target is then a silent baseline.

Delivery: notifications are committed to an outbox with the evaluations
that produced them and delivered by a background worker, so a push never
waits on a receiver and a restart does not lose queued messages (unless the
server was down so long that they have passed the 8 hour limit below, when
they are dropped unsent). A failed delivery (an error, a timeout of 2s, any
non-2xx status, **including redirects**, which are not followed) is retried with exponential backoff from 30s, up to 8
attempts (about an hour), separately per sink. A receiver that answers
`429` or `503` with a `Retry-After` header (seconds or an HTTP date) is
left alone for that delay, capped at an hour: the sink is not called for
that message or for any other message queued for it (which are put back
without counting an attempt), so a burst after a fleet-wide pass does not
hammer a rate-limited receiver or use up its messages' attempts. The wait
replaces a shorter backoff, so the attempts of one message may span several
hours. Because a held sink is called only once per hold, the messages queued
behind it would otherwise drain one per hold: so a message is **given up
(logged, not sent) once it has been queued for 8 hours**, whatever its
attempts, and that holds for every queued message. A receiver limited for
good therefore loses notifications older than 8 hours, not the newest. The
hold is kept in memory: a restart, or another replica, forgets it and finds
out with the next call. Delivery is **at least once**:
the same notification may arrive more than once, so deduplicate on
`deliveryId`, which is the same on every retry and for every sink.

There is **one notification per cluster per evaluation pass**, grouping all
targets: a change found for several targets with the same title and detail
(an EOL add-on is a blocker for every target) is one entry listing them; a
finding whose wording names the target keeps one entry per target, each in
its own words. Each notification lists at most 5
new blockers and 5 eol-approaching changes; the rest are counted in
`omitted`.

### Webhook payload

The generic webhook sends a versioned JSON body, `schemaVersion` 1, with
lowercase keys: `deliveryId`, `type` (`readiness.changed`), `timestamp`,
`cluster`, the verdict of every changed target in `targets`, the
`changes` and, when the per-kind cap dropped some, `omitted`. Servers
before schemaVersion 1 sent one PascalCase event per change and target
(`Cluster`, `Target`, `Kind`, `Title`, `Detail`); receivers written for
that shape must be updated.

With `--webhook-secret` (`$UPGRADESCOPE_WEBHOOK_SECRET`,
`--webhook-secret-file`), every delivery carries
`X-Upgradescope-Signature: sha256=<hex HMAC-SHA256(secret, raw body)>`.

Every field, the headers, how to verify the signature, and the JSON
Schema the body validates against are in the
[webhook reference](reference/webhook.md).

### Slack

Slack gets the same notification as text: one line for a single change,
`[upgradescope] <cluster> → <targets>: <kind>: <title>`, otherwise a
header line and one bullet per change.

## CSV export

`GET /api/v1/clusters/{id}/export?format=csv&target=` exports the current
stored evaluation (the HTML format carries the same verdict with a score
history). Columns:

```
cluster,target,evaluatedAt,kbVersion,verdict,score,severity,category,key,title,detail,remediation,teams,namespaces,citations
```

Every row carries the cluster, target, evaluation time, KB version,
verdict and score. The `severity` column types the rows:

- `summary`: always the first row, so a clean cluster's export is not a
  bare header. The title states the verdict and score; the detail counts
  blockers, warnings and capabilities not assessed.
- `blocker`, `warning`, `info`: one row per finding.
- `not-assessed`: one row per capability the evaluation could not assess
  (`key` is the capability, `detail` the reason). A required one makes the
  verdict `unknown`, never `ready`.

Teams, namespaces and citations are `;`-joined. A spreadsheet may split
a field again on `,`, `;`, tab or a line break (Excel imports with `;` in
many locales), so wherever a cell could start, at a field's start or
after any of those, past any white space (no-break and ideographic
spaces included), a formula trigger (`=`, `+`, `-`, `@`, tab, CR, or
their full-width forms) gets a `'` in front of it and is read as text.
An import told to split on spaces as well (LibreOffice offers it) is not
guarded: every ` -` in prose would be marked.
