DROP INDEX CONCURRENTLY IF EXISTS public.benchmark_result_submission_key_index;

ALTER TABLE public.benchmark_result
    DROP CONSTRAINT IF EXISTS benchmark_result_submission_idempotency_check,
    DROP COLUMN IF EXISTS submission_payload_sha256,
    DROP COLUMN IF EXISTS submission_key;
