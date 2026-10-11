-- 0011_evaluation_baseline.sql (SQLite) — an unknown verdict can be a
-- notification baseline (#380).
-- baseline: 1 when the server judged the evaluation a notification
-- baseline: a decided verdict (ready, or a blocker), or an unknown one
-- whose required gaps hide no finding (kb-coverage: the target is past
-- the knowledge base's horizon; target: it is not an upgrade), so the pass
-- saw everything a blocker could come from. 0 for an unknown verdict
-- behind a required gap that hides findings (a collector failure). NULL
-- marks a row written before this migration, or by a binary that predates
-- it (a rollback after it ran): it is a baseline only when decided, as
-- before. Rows are not backfilled: an unknown row written before had no
-- delta computed, and nothing carried forward, so it is no baseline.
-- idx_evaluations_baseline replaces 0009's idx_evaluations_decided with
-- the new condition: the newest baseline of a (cluster, target) is one
-- step away however long a stretch of other unknown verdicts lies on top
-- of it (LatestKnownEvaluation; retention keeps it; the commit checks
-- it). Same column and index as pgmigrations/0011, but INTEGER 0/1 ↔
-- BOOLEAN; the Postgres one is pre-buildable (IF NOT EXISTS), and SQLite
-- has no ADD COLUMN IF NOT EXISTS, nor a concurrent build to need it.

ALTER TABLE evaluations ADD COLUMN baseline INTEGER;
CREATE INDEX IF NOT EXISTS idx_evaluations_baseline ON evaluations (cluster_id, target, id)
    WHERE ready = 1 OR blockers > 0 OR baseline = 1;
DROP INDEX IF EXISTS idx_evaluations_decided;
