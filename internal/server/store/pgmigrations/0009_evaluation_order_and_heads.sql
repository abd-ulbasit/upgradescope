-- 0009_evaluation_order_and_heads.sql (Postgres) — what reads and the
-- background pass need, without a report or an inventory (#241).
-- idx_evaluations_cluster_target_id: "newest evaluation by id" of a
-- (cluster, target) (LatestKnownEvaluation, the notification baseline;
-- LatestEvaluation; ScoreHistory) walks this index backwards. With only
-- (cluster_id, target, created_at) every history row of the pair was
-- sorted, its report carried along. It replaces that index: nothing
-- orders by created_at within a pair (a clock step can reorder it), and
-- retention filters on created_at alone.
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
-- this migration (an old replica mid-rollout, or a rollback): it is read
-- as carrying one, which costs that cluster's inventory on the next
-- pass, never a hold. Existing rows are backfilled from their reports.
-- server_version: backfilled from each stored inventory's serverVersion
-- where 0006 left it '', one row at a time so that an inventory jsonb
-- refuses (one that does not parse, or holds \u0000) stays '' instead of
-- failing the migration, as in 0007; so no read decodes an inventory to
-- learn the version a snapshot is judged at.
-- Same indexes, column and backfills as migrations/0009, but
-- BOOLEAN ↔ INTEGER 0/1.

CREATE INDEX idx_evaluations_cluster_target_id ON evaluations (cluster_id, target, id);
DROP INDEX idx_evaluations_cluster_target_created;
CREATE INDEX idx_evaluations_decided ON evaluations (cluster_id, target, id)
    WHERE ready OR blockers > 0;
CREATE INDEX idx_evaluations_snapshot_target_id ON evaluations (snapshot_id, target, id);
DROP INDEX idx_evaluations_snapshot_target;

ALTER TABLE evaluations ADD COLUMN carries_hold BOOLEAN;
UPDATE evaluations SET carries_hold =
    (report IS NOT NULL AND position(convert_to('"holdUntil":', 'UTF8') IN report) > 0);

DO $$
DECLARE
    r   RECORD;
    doc jsonb;
BEGIN
    FOR r IN SELECT id, inventory FROM snapshots WHERE server_version = '' LOOP
        BEGIN
            doc := convert_from(r.inventory, 'UTF8')::jsonb;
        EXCEPTION WHEN others THEN
            doc := NULL;
        END;
        IF jsonb_typeof(doc) = 'object' AND jsonb_typeof(doc -> 'serverVersion') = 'string' THEN
            UPDATE snapshots SET server_version = doc ->> 'serverVersion' WHERE id = r.id;
        END IF;
    END LOOP;
END
$$;
