-- 0010_evaluation_created_at.sql (Postgres): the index retention's
-- evaluation drain reads (#297). Prune deletes evaluations older than the
-- cutoff in batches of (created_at, id) order; 0009 dropped the only index
-- with created_at in it. With this index the drain's inner SELECT
-- (ORDER BY created_at, id LIMIT n) walks it from the oldest entry and
-- stops at the cutoff, so a run with nothing to delete reads no report.
-- Same index as migrations/0010.

CREATE INDEX idx_evaluations_created_at ON evaluations (created_at, id);
