# Retention and backup

What the server keeps, how it prunes it, how much room it needs, and how to
back it up. Only the server stores anything: the agent and `scan` keep no
state beyond the `ClusterReadiness` object.

## Retention and sizing

Every snapshot whose inventory changed is stored, with one evaluation per
target, and every re-evaluation whose result changed adds an evaluation.
Unchanged pushes are deduplicated, so storage grows with the number of real
inventory changes (node, image and namespace churn included), at most one
per agent tick (144 a day at the default 10m interval).

`serve --retention` (default `90d`; whole days or a Go duration of at least
a day; `0` keeps everything) prunes once at startup and then daily:

- evaluations created before the window, then
- snapshots received before the window that no evaluation refers to any
  more,
- except each cluster's latest snapshot and its evaluations, however old:
  a cluster that has been silent longer than the window keeps its current
  state (and shows as stale).

An older snapshot stays while any of its evaluations is inside the window.
Score history, the dashboard sparkline and exports reach back the window
at most. Each run that deletes rows logs the counts. Measured in
`internal/server/store/sqlite_lifecycle_test.go`: one cluster whose
inventory changed every day for a year (365 snapshots, 730 evaluations
with two targets) plus one cluster silent all year (1 and 1) go from 366
snapshots and 731 evaluations to 92 and 183 under a 90-day window: 91 days
of the busy cluster, and the silent cluster's current state.

Sizing: the October 2026 audit measured about 35 KB per changed snapshot
for a realistic inventory (60 nodes, 250 namespaces, 80 Helm releases),
plus one report per target. Plan for
`clusters × changes per day × retention days × (snapshot + targets × report)`.
The [scale benchmark](scale.md#the-server) measured, for 200 generated
clusters averaging 28 KiB and three targets, about 100 KiB per changed
snapshot on SQLite and 19 KiB on Postgres, which compresses large values.
SQLite reuses the pages pruning frees, but the file does not shrink; run
`sqlite3 upgradescope.sqlite VACUUM` with the server stopped to return the
space. A large or busy fleet belongs on Postgres (`--db-url`, chart value
`server.database.existingSecret`), which also allows several replicas.

Fleet reads stay small as the fleet grows: `/api/v1/fleet` and the cluster
list read each cluster's latest snapshot id and server version, never its
inventory. `make bench-server` seeds 500 clusters with ~35 KiB inventories
on SQLite and runs 10 concurrent `/fleet` readers; it fails above a 1s p95
or a 512 MiB heap peak. On an arm64 Mac (October 2026): p95 about 0.5s,
peak live heap 15 MiB (it was 1.1 GiB and 1.2s while every request
decoded every inventory).

## What the history records

`GET /api/v1/clusters/{id}/history` is a change log, not a sample taken at
every evaluation: it returns one point (`at`, `score`, `ready`) per stored
evaluation of the target, and a row is stored only when the result is new.

- **A row is added** when a pushed snapshot differs from the cluster's
  latest (one row per target, whatever its score), when a re-evaluation
  changes the result, or when a target has no evaluation yet (a new
  `serve --targets` minor). A re-evaluation runs on a duplicate push and in
  the background pass, hourly and at UTC midnight, for a stored evaluation
  whose knowledge-base version or team map differs from the server's, or
  that was evaluated before today (EOL dates are day-granular). The result
  has changed when its verdict, its score or its set of findings (severity
  and finding key) differs from the stored row's.
- **A row is refreshed in place** when the re-evaluation gives the same
  result. Its report, knowledge-base version, team-map hash and
  `evaluatedAt` are rewritten and no point is added, so `evaluatedAt` in
  `/report` and the exports moves on while `/history` shows nothing new.
- **`at` is the server's clock** when the row was first stored, at ingest or
  at the re-evaluation that found the change. It is not when the agent
  observed the cluster, and it does not follow a later refresh. A change
  made while the server was down is recorded when the server next hears of
  it.

Team attribution is part of the stored report, not of what is compared. A
team-map change that leaves every verdict, score and finding as it was
rewrites the row's report in place with the new teams: the earlier
attribution is gone from the database as well as from the API, and
`/history` keeps only `at`, `score` and `ready`. `/report`, `/teams` and the
exports always read the latest stored evaluation, so they cannot answer
which team owned a blocker at an earlier time. An auditor who needs that
keeps the exports from the time (`GET /api/v1/clusters/{id}/export`).

## Backup and restore

The database holds each cluster's registration (name bound to its cluster
UID), the stored snapshots and evaluations (score history, exports), the
notification outbox and the hashes of per-cluster ingest tokens. Losing it
loses history, not the present: every agent pushes its full inventory at
least hourly (`--force-sync-every`), so the fleet view refills within about
an hour of a fresh start, and the `ClusterReadiness` objects in each cluster
are untouched. What does not come back is history, the per-cluster tokens
(mint and roll out new ones) and the bindings of names to cluster UIDs.

**SQLite.** The database is one file plus its `-wal` and `-shm` files, in
WAL mode. Copy it with the server stopped, so the three files are
consistent:

```sh
# chart install: the PVC is mounted at /data
kubectl -n upgradescope scale deploy/upgradescope-server --replicas=0
# copy /data/upgradescope.sqlite* off the volume, e.g. with a pod that mounts the PVC
kubectl -n upgradescope scale deploy/upgradescope-server --replicas=1
```

The server image is distroless (no shell, no `sqlite3`), so a copy needs a
helper pod that mounts the PVC, or a volume snapshot of the PVC
(`VolumeSnapshot`, where your CSI driver supports it). Restore by putting
the files back with the server stopped. The schema migrates forward on
startup, so a backup taken with an older release opens in a newer one;
the reverse is not supported.

**Postgres.** Use your usual tooling (`pg_dump`, or your provider's
snapshots and point-in-time recovery). The server holds no state outside
the database, so any consistent backup of it is a complete one.
