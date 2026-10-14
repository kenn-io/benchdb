-- Each stored track records how far its latest point moved from its history
-- window's mean, in percent of that mean, so the Benchmarks list can show the
-- size of a change next to its status. Re-queue every stored benchmark so the
-- new column fills in.
ALTER TABLE benchmark_verdict ADD COLUMN change_percent double precision;

INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', benchmark_id
FROM benchmark_verdict
ORDER BY 2
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
