-- 0007_evaluation_not_assessed.sql (Postgres) — what a verdict could not
-- cover, beside the report.
-- not_assessed: the report's notAssessed array as JSON, '' when it has
-- none. The summaries of every cluster (cluster list, fleet matrix,
-- metrics) read it instead of the report, which can be as large as the
-- snapshot. NULL marks a row inserted by a binary that predates this
-- migration (an old replica mid-rollout, or a rollback); it reads as none
-- until the next pass refreshes the row. Such a binary's refresh of an
-- existing row leaves not_assessed as it was, so that row shows its
-- earlier gaps until a newer binary writes it again.
-- Existing rows are backfilled from their reports, one row at a time so
-- that a report jsonb refuses gets '' instead of failing the migration:
-- one that does not parse, as the read path treated it, and one whose
-- strings hold \u0000, which encoding/json accepts but jsonb does not, so
-- its gaps read as none until the row is written again.
-- Same column and backfill as migrations/0007.

ALTER TABLE evaluations ADD COLUMN not_assessed TEXT;

DO $$
DECLARE
    r   RECORD;
    doc jsonb;
BEGIN
    FOR r IN SELECT id, report FROM evaluations LOOP
        BEGIN
            doc := convert_from(r.report, 'UTF8')::jsonb;
        EXCEPTION WHEN others THEN
            doc := NULL;
        END;
        UPDATE evaluations SET not_assessed = CASE
            WHEN jsonb_typeof(doc) = 'object'
             AND jsonb_typeof(doc -> 'notAssessed') = 'array'
             AND jsonb_array_length(doc -> 'notAssessed') > 0
                THEN (doc -> 'notAssessed')::text
            ELSE ''
        END
        WHERE id = r.id;
    END LOOP;
END
$$;
