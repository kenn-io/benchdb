ALTER TABLE benchmark_verdict
    DROP COLUMN point_timestamps,
    DROP COLUMN preview_points,
    ADD COLUMN history_fingerprints text[] NOT NULL DEFAULT '{}';
ALTER TABLE benchmark_verdict ALTER COLUMN history_fingerprints DROP DEFAULT;

INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', benchmark_id
FROM benchmark_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
