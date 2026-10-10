-- 0010_evaluation_created_at.sql (SQLite): the index retention's evaluation
-- drain reads (#297). Prune deletes evaluations older than the cutoff in
-- batches of (created_at, id) order; 0009 dropped the only index with
-- created_at in it, so every run, the last short batch included, scanned
-- every evaluation row to find the old ones, and created_at sits after
-- the report BLOB, so that read each report's overflow pages, under
-- SQLite's single write lock. With this index the drain's inner SELECT
-- (ORDER BY created_at, id LIMIT n) walks it from the oldest entry and
-- stops at the cutoff, whatever the reports weigh.
-- Same index as pgmigrations/0010.

CREATE INDEX idx_evaluations_created_at ON evaluations (created_at, id);
