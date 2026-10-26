DELETE FROM ci_run_report WHERE evaluation > 1;
ALTER TABLE ci_run_report DROP CONSTRAINT ci_run_report_pkey;
ALTER TABLE ci_run_report DROP COLUMN evaluation;
ALTER TABLE ci_run_report ADD PRIMARY KEY (run_id);
