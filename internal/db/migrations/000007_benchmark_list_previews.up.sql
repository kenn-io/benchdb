-- The benchmark list drew each row's chart and point count from the
-- benchmark's history on every page load, which grew with history length.
-- Each machine track now stores the commit time of every history point and
-- its latest chart points, which the verdict worker recomputes with the
-- status, so the list reads only stored rows.
ALTER TABLE benchmark_verdict
    DROP COLUMN history_fingerprints,
    ADD COLUMN point_timestamps timestamp without time zone[] NOT NULL DEFAULT '{}',
    ADD COLUMN preview_points jsonb NOT NULL DEFAULT '[]';
ALTER TABLE benchmark_verdict
    ALTER COLUMN point_timestamps DROP DEFAULT,
    ALTER COLUMN preview_points DROP DEFAULT;

-- Recompute every stored track so the new columns fill in.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', benchmark_id
FROM benchmark_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
