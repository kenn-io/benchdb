-- Pull-request comparisons now use the main commit a merged-main run was
-- measured on as their baseline, and a change must also move away from its
-- own baseline result to count. Re-queue every stored run verdict so the Runs
-- page stops showing flags the old rule produced.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'run', run_id
FROM run_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
