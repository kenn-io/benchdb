-- Re-queue every stored run verdict so it is rescored by the rule the
-- rolled-back code applies.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'run', run_id
FROM run_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
