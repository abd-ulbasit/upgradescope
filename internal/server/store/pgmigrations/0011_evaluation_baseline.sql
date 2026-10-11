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
-- idx_evaluations_decided keeps its name and takes the new condition:
-- the newest baseline of a (cluster, target) is one step away however
-- long a stretch of other unknown verdicts lies on top of it
-- (LatestKnownEvaluation; retention keeps it; the commit checks it).
-- Same column and index as migrations/0011, but BOOLEAN ↔ INTEGER 0/1.

ALTER TABLE evaluations ADD COLUMN baseline BOOLEAN;
DROP INDEX idx_evaluations_decided;
CREATE INDEX idx_evaluations_decided ON evaluations (cluster_id, target, id)
    WHERE ready OR blockers > 0 OR baseline;
