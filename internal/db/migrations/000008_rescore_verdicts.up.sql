-- Pull-request comparisons now use the main commit a merged-main run was
-- measured on as their baseline, and a change must also move away from its own
-- baseline result to count. Benchmark statuses now follow only the contexts
-- measured at each machine's latest commit. Re-queue every stored verdict so
-- the Runs and Benchmarks pages stop showing what the old rules produced.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'run', run_id
FROM run_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();

INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', benchmark_id
FROM benchmark_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
