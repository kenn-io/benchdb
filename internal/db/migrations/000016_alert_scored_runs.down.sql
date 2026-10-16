DROP TABLE alert_scored_run;
ALTER TABLE alert_rule
  DROP COLUMN evaluation_lease_until,
  DROP COLUMN report_start_at;
