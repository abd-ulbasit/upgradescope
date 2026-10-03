-- 0008_read_scopes.sql (Postgres) — team-scoped read access (#72).
-- read_tokens: bearer tokens for the read API, each scoped to teams, a
-- JSON array of team names, sorted, or ["*"] for the whole fleet.
-- token_hash is the hex sha256 of the plaintext (never stored), UNIQUE so
-- one plaintext has one scope; token_prefix as for ingest tokens.
-- revoked_at NULL == active. Rows are never deleted: once one exists the
-- server requires a read token, and revoking the last one must not open
-- the read API again.
-- evaluations.teams: the teams the evaluated inventory attributes a
-- namespace to, a JSON array (TEXT, as not_assessed), '[]' for none. A
-- scoped token reads the clusters whose current evaluations name one of
-- its teams. NULL marks a row written before this migration (an old
-- replica mid-rollout, or a rollback): the server treats it as stale, so
-- its first re-evaluation pass, at startup, rewrites it; until then no
-- scoped token reads that cluster.
-- Divergence from migrations/0008_read_scopes.sql: BIGSERIAL ↔
-- AUTOINCREMENT, TIMESTAMPTZ ↔ TEXT times.

CREATE TABLE read_tokens (
    id           BIGSERIAL PRIMARY KEY,
    teams        TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ
);

ALTER TABLE evaluations ADD COLUMN teams TEXT;
