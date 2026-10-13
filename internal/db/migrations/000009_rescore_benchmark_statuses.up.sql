-- A Benchmarks status now also needs the latest point to differ from its
-- history window's mean by more than 2%. Re-queue every stored benchmark
-- verdict so the Benchmarks page stops showing what the old rule produced.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', benchmark_id
FROM benchmark_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
