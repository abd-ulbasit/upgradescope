-- 0001_init.sql (Postgres) — clusters / snapshots / evaluations.
-- Deliberate divergence from migrations/0001_init.sql (SQLite): BIGSERIAL
-- instead of INTEGER AUTOINCREMENT, TIMESTAMPTZ instead of RFC 3339 TEXT
-- (the store layer converts to/from time.Time and returns UTC), BYTEA
-- instead of BLOB, BOOLEAN instead of INTEGER 0/1. Semantics are identical
-- and pinned by storetest.RunStoreConformance; ids stay monotonic with
-- insertion order ("latest snapshot" relies on max(id)).

CREATE TABLE clusters (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    cluster_uid TEXT NOT NULL DEFAULT '',
    first_seen  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL
);

CREATE TABLE snapshots (
    id            BIGSERIAL PRIMARY KEY,
    cluster_id    BIGINT NOT NULL REFERENCES clusters(id),
    hash          TEXT   NOT NULL,
    kb_version    TEXT   NOT NULL DEFAULT '',
    agent_version TEXT   NOT NULL DEFAULT '',
    received_at   TIMESTAMPTZ NOT NULL,
    inventory     BYTEA  NOT NULL
);

CREATE INDEX idx_snapshots_cluster_received
    ON snapshots (cluster_id, received_at);

CREATE TABLE evaluations (
    id          BIGSERIAL PRIMARY KEY,
    cluster_id  BIGINT NOT NULL REFERENCES clusters(id),
    snapshot_id BIGINT NOT NULL REFERENCES snapshots(id),
    target      TEXT   NOT NULL,
    kb_version  TEXT   NOT NULL DEFAULT '',
    score       INTEGER NOT NULL,
    ready       BOOLEAN NOT NULL,
    blockers    INTEGER NOT NULL,
    warnings    INTEGER NOT NULL,
    report      BYTEA,
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_evaluations_cluster_target_created
    ON evaluations (cluster_id, target, created_at);
