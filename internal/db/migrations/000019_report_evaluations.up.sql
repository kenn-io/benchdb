ALTER TABLE ci_run_report ADD COLUMN evaluation integer NOT NULL DEFAULT 1 CHECK (evaluation > 0);
ALTER TABLE ci_run_report DROP CONSTRAINT ci_run_report_pkey;
ALTER TABLE ci_run_report ADD PRIMARY KEY (run_id, evaluation);
