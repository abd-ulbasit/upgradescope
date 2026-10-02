-- 0006_snapshot_server_version.sql (Postgres) — the version a snapshot is
-- judged at.
-- server_version: the inventory's own serverVersion, or, for a degraded
-- push that reported none (the versions collector failed), the previous
-- snapshot's, so the cluster keeps its default target and fleet cell.
-- Existing rows get '' and readers fall back to the stored inventory's
-- serverVersion (no backfill, as in migrations/0006).
-- Identical to migrations/0006.

ALTER TABLE snapshots ADD COLUMN server_version TEXT NOT NULL DEFAULT '';
