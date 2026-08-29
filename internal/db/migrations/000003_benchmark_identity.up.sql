ALTER TABLE public.benchmark_result
    ADD COLUMN benchmark_id text
    GENERATED ALWAYS AS (md5(case_id || commit_repo_url)) STORED NOT NULL;

CREATE INDEX CONCURRENTLY benchmark_result_benchmark_id_commit_id_index
    ON public.benchmark_result (benchmark_id, commit_id)
    WHERE error IS NULL;

CREATE INDEX CONCURRENTLY commit_default_branch_timestamp_index
    ON public.commit ("timestamp" DESC, id DESC)
    WHERE "timestamp" IS NOT NULL AND sha = fork_point_sha;
