-- Alert rules score every finalized run for their repository, not only the
-- newest one. report_start_at is the finalize time of the newest run when the
-- rule started, so history before it is not replayed. alert_scored_run
-- records each run a rule has scored, so a run is scored once even when
-- reports become visible out of finalize order.
ALTER TABLE alert_rule ADD COLUMN report_start_at timestamptz;

CREATE TABLE alert_scored_run (
  rule_id character varying(50) NOT NULL REFERENCES alert_rule (id) ON DELETE CASCADE,
  run_id text NOT NULL,
  scored_at timestamptz NOT NULL,
  PRIMARY KEY (rule_id, run_id)
);
