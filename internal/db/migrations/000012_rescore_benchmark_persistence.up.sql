-- A Benchmarks status now needs at least ten earlier results, and a
-- regression or improvement has to hold for the two latest results. Re-queue
-- every stored benchmark so the Benchmarks page stops showing one-off spikes.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', benchmark_id
FROM benchmark_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
