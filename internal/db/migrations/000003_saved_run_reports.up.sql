CREATE TABLE ci_run_report (
    run_id text PRIMARY KEY,
    result_ids text[] NOT NULL,
    evaluated_at timestamptz NOT NULL,
    report jsonb NOT NULL,
    summary jsonb NOT NULL
);
