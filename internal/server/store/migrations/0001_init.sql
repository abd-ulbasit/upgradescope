-- 0001_init.sql — clusters / snapshots / evaluations.
-- Times are TEXT: RFC 3339 UTC with fixed nine-digit fractional seconds,
-- so lexicographic order == instant order. Booleans are INTEGER 0/1.
-- AUTOINCREMENT guarantees ids are strictly monotonic (insertion order):
-- "latest snapshot" relies on max(id).

CREATE TABLE clusters (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    cluster_uid TEXT    NOT NULL DEFAULT '',
    first_seen  TEXT    NOT NULL,
    last_seen   TEXT    NOT NULL
);

CREATE TABLE snapshots (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    cluster_id    INTEGER NOT NULL REFERENCES clusters(id),
    hash          TEXT    NOT NULL,
    kb_version    TEXT    NOT NULL DEFAULT '',
    agent_version TEXT    NOT NULL DEFAULT '',
    received_at   TEXT    NOT NULL,
    inventory     BLOB    NOT NULL
);

CREATE INDEX idx_snapshots_cluster_received
    ON snapshots (cluster_id, received_at);

CREATE TABLE evaluations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    cluster_id  INTEGER NOT NULL REFERENCES clusters(id),
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
    target      TEXT    NOT NULL,
    kb_version  TEXT    NOT NULL DEFAULT '',
    score       INTEGER NOT NULL,
    ready       INTEGER NOT NULL,
    blockers    INTEGER NOT NULL,
    warnings    INTEGER NOT NULL,
    report      BLOB,
    created_at  TEXT    NOT NULL
);

CREATE INDEX idx_evaluations_cluster_target_created
    ON evaluations (cluster_id, target, created_at);
