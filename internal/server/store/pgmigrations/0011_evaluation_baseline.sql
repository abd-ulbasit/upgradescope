-- 0011_evaluation_baseline.sql (Postgres) — an unknown verdict can be a
-- notification baseline (#380).
-- baseline: TRUE when the server judged the evaluation a notification
-- baseline: a decided verdict (ready, or a blocker), or an unknown one
-- whose required gaps hide no finding (kb-coverage: the target is past
-- the knowledge base's horizon; target: it is not an upgrade), so the pass
-- saw everything a blocker could come from. FALSE for an unknown verdict
-- behind a required gap that hides findings (a collector failure). NULL
-- marks a row written before this migration, or by a binary that predates
-- it (a rollback after it ran): it is a baseline only when decided, as
-- before. Rows are not backfilled: an unknown row written before had no
-- delta computed, and nothing carried forward, so it is no baseline.
-- idx_evaluations_baseline replaces 0009's idx_evaluations_decided with
-- the new condition: the newest baseline of a (cluster, target) is one
-- step away however long a stretch of other unknown verdicts lies on top
-- of it (LatestKnownEvaluation; retention keeps it; the commit checks
-- it). The old replicas' condition (ready OR blockers > 0) implies the
-- new one, so their queries use the new index during a rolling upgrade.
-- Same column and index as migrations/0011, but BOOLEAN ↔ INTEGER 0/1.
--
-- IF NOT EXISTS (column and index) and a new index name so an operator
-- can add the column and build the index first with CREATE INDEX
-- CONCURRENTLY, which does not block writes the way the CREATE INDEX of a
-- migration does for as long as the build takes (a migration runs in a
-- transaction, which CONCURRENTLY cannot); the migration then finds both
-- and only drops the old index (docs/operations/upgrade.md).

ALTER TABLE evaluations ADD COLUMN IF NOT EXISTS baseline BOOLEAN;
CREATE INDEX IF NOT EXISTS idx_evaluations_baseline ON evaluations (cluster_id, target, id) WHERE ready OR blockers > 0 OR baseline;
DROP INDEX IF EXISTS idx_evaluations_decided;
