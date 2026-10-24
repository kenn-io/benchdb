-- Benchmark statuses now honor a practical tolerance the latest result sets.
-- Re-queue every stored benchmark so the Benchmarks page stops showing
-- statuses the old rule produced.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', benchmark_id
FROM benchmark_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
