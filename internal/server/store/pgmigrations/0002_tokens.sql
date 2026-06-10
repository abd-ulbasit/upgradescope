-- 0002_tokens.sql (Postgres) — per-cluster ingest tokens (spec §8).
-- cluster_name is deliberately NOT a foreign key: tokens are minted before
-- the cluster's first push creates its clusters row. token_hash is the hex
-- sha256 of the plaintext (never stored), UNIQUE so one plaintext cannot
-- map to two clusters. revoked_at NULL == active.
-- Divergence from migrations/0002_tokens.sql: BIGSERIAL↔AUTOINCREMENT,
-- TIMESTAMPTZ↔TEXT times.

CREATE TABLE tokens (
    id           BIGSERIAL PRIMARY KEY,
    cluster_name TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX idx_tokens_cluster ON tokens (cluster_name);
