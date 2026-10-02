-- 0005_cluster_lifecycle.sql (Postgres) — cluster deletion and retention.
-- outbox.cluster_id: the cluster a queued notification is about, so
-- deleting a cluster drops its undelivered notifications too. Rows queued
-- before this migration get 0 (no cluster) and are delivered as before.
-- idx_snapshots_cluster_id: "latest snapshot" is MAX(id) per cluster, read
-- on every fleet and report request and by retention.
-- Identical to migrations/0005 except BIGINT↔INTEGER.

ALTER TABLE outbox ADD COLUMN cluster_id BIGINT NOT NULL DEFAULT 0;
CREATE INDEX idx_outbox_cluster ON outbox (cluster_id);

CREATE INDEX idx_snapshots_cluster_id ON snapshots (cluster_id, id);
