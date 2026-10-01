-- 0004_evaluation_freshness_outbox.sql (SQLite) — evaluation freshness and
-- the notification outbox.
-- evaluated_at: when the row's result was last confirmed. A re-evaluation
-- with the same verdict, score and finding keys refreshes it instead of
-- adding a history row (created_at stays the history point). Existing rows
-- were last evaluated when created. A pre-0004 binary (after a rollback)
-- inserts without it, leaving '': reads treat that as created_at.
-- team_map_hash: the server --team-map the report was computed with, so an
-- edited map triggers a re-evaluation ('' = none, or a pre-0004 row).
-- outbox: notifications committed with their evaluations and delivered
-- after commit, one row per (event, sink), with bounded retries.
-- Divergence from pgmigrations/0004: TEXT times↔TIMESTAMPTZ,
-- AUTOINCREMENT↔BIGSERIAL, BLOB↔BYTEA, evaluated_at DEFAULT ''↔now().

ALTER TABLE evaluations ADD COLUMN evaluated_at TEXT NOT NULL DEFAULT '';
UPDATE evaluations SET evaluated_at = created_at;
ALTER TABLE evaluations ADD COLUMN team_map_hash TEXT NOT NULL DEFAULT '';

-- Read paths look up evaluations of one snapshot.
CREATE INDEX idx_evaluations_snapshot_target
    ON evaluations (snapshot_id, target, created_at);

CREATE TABLE outbox (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    sink            TEXT    NOT NULL,
    payload         BLOB    NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL,
    next_attempt_at TEXT    NOT NULL,
    last_error      TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_outbox_next_attempt ON outbox (next_attempt_at);
