-- Comparisons now need ten results in their lookback window before they call
-- a regression or improvement. Re-queue every stored run verdict so the Runs
-- page stops showing what the old rule produced. Benchmark statuses already
-- required ten earlier results.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'run', run_id
FROM run_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
