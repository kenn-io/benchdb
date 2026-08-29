DROP INDEX CONCURRENTLY public.commit_default_branch_timestamp_index;
DROP INDEX CONCURRENTLY public.benchmark_result_benchmark_id_commit_id_index;

ALTER TABLE public.benchmark_result
    DROP COLUMN benchmark_id;
