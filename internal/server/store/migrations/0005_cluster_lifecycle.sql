-- 0005_cluster_lifecycle.sql (SQLite) — cluster deletion and retention.
-- outbox.cluster_id: the cluster a queued notification is about, so
-- deleting a cluster drops its undelivered notifications too. Rows queued
-- before this migration get 0 (no cluster) and are delivered as before.
-- idx_snapshots_cluster_id: "latest snapshot" is MAX(id) per cluster; with
-- only (cluster_id, received_at) indexed, SQLite sorted every snapshot of
-- the cluster in a temp B-tree, reading each inventory blob, on every
-- fleet and report read. Retention's per-cluster MAX(id) uses it too.
-- Identical to pgmigrations/0005 except INTEGER↔BIGINT.

ALTER TABLE outbox ADD COLUMN cluster_id INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_outbox_cluster ON outbox (cluster_id);

CREATE INDEX idx_snapshots_cluster_id ON snapshots (cluster_id, id);
