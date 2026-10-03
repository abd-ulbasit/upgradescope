-- 0007_evaluation_not_assessed.sql (SQLite) — what a verdict could not
-- cover, beside the report.
-- not_assessed: the report's notAssessed array as JSON, '' when it has
-- none. The summaries of every cluster (cluster list, fleet matrix,
-- metrics) read it instead of the report, which can be as large as the
-- snapshot. NULL marks a row inserted by a binary that predates this
-- migration (a rollback after it ran); it reads as none until the next
-- pass refreshes the row. Such a binary's refresh of an existing row
-- leaves not_assessed as it was, so that row shows its earlier gaps until
-- a newer binary writes it again.
-- Existing rows are backfilled from their reports. CAST makes the BLOB
-- JSON text (SQLite's JSON functions refuse BLOBs); a report that does
-- not parse gets '', as the read path treated it.
-- Same column and backfill as pgmigrations/0007.

ALTER TABLE evaluations ADD COLUMN not_assessed TEXT;

UPDATE evaluations SET not_assessed = CASE
    WHEN report IS NULL OR NOT json_valid(CAST(report AS TEXT)) THEN ''
    WHEN json_type(CAST(report AS TEXT), '$.notAssessed') = 'array'
     AND json_array_length(CAST(report AS TEXT), '$.notAssessed') > 0
        THEN json_extract(CAST(report AS TEXT), '$.notAssessed')
    ELSE ''
END;
