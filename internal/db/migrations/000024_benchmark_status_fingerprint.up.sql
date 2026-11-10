-- The benchmark list names each machine track's status, but not the series
-- that sets it. Clients that follow a regressed benchmark to its results had
-- to list series, which scores the whole archive on every request. Each track
-- now stores that series' fingerprint, which the verdict worker recomputes
-- with the status.
ALTER TABLE benchmark_verdict ADD COLUMN status_fingerprint text;

-- Recompute every stored track so the new column fills in.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', benchmark_id
FROM benchmark_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
