-- 0009_evaluation_order_and_heads.sql (SQLite) — what reads and the
-- background pass need, without a report or an inventory (#241).
-- idx_evaluations_cluster_target_id: "newest evaluation by id" of a
-- (cluster, target) (LatestKnownEvaluation, the notification baseline;
-- LatestEvaluation; ScoreHistory) walks this index backwards. With only
-- (cluster_id, target, created_at) every history row of the pair went
-- through a temp B-tree, its report carried along. It replaces that
-- index: nothing orders by created_at within a pair (a clock step can
-- reorder it), and retention filters on created_at alone.
-- idx_evaluations_decided: the same for the decided evaluations alone
-- (ready, or a blocker), so the notification baseline is one step away
-- however long a stretch of unknown verdicts lies on top of it; retention
-- keeps each pair's newest one (Prune) and the commit checks it
-- (EvaluationBatch.Expect) through it too.
-- idx_evaluations_snapshot_target_id: the same for the evaluations of
-- one snapshot (CurrentEvaluation and its summary, the commit's
-- conflict checks), replacing (snapshot_id, target, created_at).
-- carries_hold: whether the report carries a deprecated caller held after
-- an apiserver restart (its carried heads' "holdUntil"), so the
-- background pass knows which evaluations need the inventory without
-- loading any report. NULL marks a row written by a binary that predates
-- this migration (a rollback after it ran): it is read as carrying one,
-- which costs that cluster's inventory on the next pass, never a hold.
-- Existing rows are backfilled from their reports.
-- server_version: backfilled from each stored inventory's serverVersion
-- where 0006 left it '' (CAST makes the BLOB JSON text, as in 0007; one
-- that does not parse, or names no version, stays ''), so no read decodes
-- an inventory to learn the version a snapshot is judged at.
-- Same indexes, column and backfills as pgmigrations/0009, but
-- INTEGER 0/1 ↔ BOOLEAN.

CREATE INDEX idx_evaluations_cluster_target_id ON evaluations (cluster_id, target, id);
DROP INDEX idx_evaluations_cluster_target_created;
CREATE INDEX idx_evaluations_decided ON evaluations (cluster_id, target, id)
    WHERE ready = 1 OR blockers > 0;
CREATE INDEX idx_evaluations_snapshot_target_id ON evaluations (snapshot_id, target, id);
DROP INDEX idx_evaluations_snapshot_target;

ALTER TABLE evaluations ADD COLUMN carries_hold INTEGER;
UPDATE evaluations SET carries_hold = CASE
    WHEN report IS NOT NULL AND instr(report, CAST('"holdUntil":' AS BLOB)) > 0 THEN 1
    ELSE 0
END;

UPDATE snapshots SET server_version = json_extract(CAST(inventory AS TEXT), '$.serverVersion')
WHERE server_version = ''
  AND json_valid(CAST(inventory AS TEXT))
  AND json_type(CAST(inventory AS TEXT), '$.serverVersion') = 'text';
