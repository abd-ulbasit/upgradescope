-- 0008_read_scopes.sql (SQLite) — team-scoped read access (#72).
-- read_tokens: bearer tokens for the read API, each scoped to teams, a
-- JSON array of team names, sorted, or ["*"] for the whole fleet.
-- token_hash is the hex sha256 of the plaintext (never stored), UNIQUE so
-- one plaintext has one scope; token_prefix as for ingest tokens.
-- revoked_at NULL == active. Rows are never deleted: once one exists the
-- server requires a read token, and revoking the last one must not open
-- the read API again.
-- evaluations.teams: the teams the evaluated inventory attributes a
-- namespace to, a JSON array, '[]' for none. A scoped token reads the
-- clusters whose current evaluations name one of its teams. NULL marks a
-- row written before this migration (or by a binary that predates it):
-- the server treats it as stale, so its first re-evaluation pass, at
-- startup, rewrites it; until then no scoped token reads that cluster.
-- Divergence from pgmigrations/0008_read_scopes.sql: AUTOINCREMENT ↔
-- BIGSERIAL, TEXT times ↔ TIMESTAMPTZ.

CREATE TABLE read_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    teams        TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    revoked_at   TEXT
);

ALTER TABLE evaluations ADD COLUMN teams TEXT;
