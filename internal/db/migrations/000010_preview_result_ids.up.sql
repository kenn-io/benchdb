-- Stored chart previews now carry each point's result ID and order points
-- that share a commit by result ID, the way the list picks a latest result.
-- Re-queue every stored benchmark verdict so its preview is rebuilt.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', benchmark_id
FROM benchmark_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
