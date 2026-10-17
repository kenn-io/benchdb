-- Alert rules score every finalized run for their repository, not only the
-- newest one. report_start_at is the finalize time of the newest run when the
-- rule started, so history before it is not replayed. alert_scored_run
-- records each run a rule has scored and when that run was finalized, so a
-- run is scored once and a run that becomes visible late cannot override a
-- newer run's outcome. evaluation_lease_until and evaluation_lease_token let
-- one evaluator at a time score a rule without holding a database connection;
-- its writes check the token, so an evaluator that lost the lease cannot write.
ALTER TABLE alert_rule
  ADD COLUMN report_start_at timestamptz,
  ADD COLUMN evaluation_lease_until timestamptz,
  ADD COLUMN evaluation_lease_token text;

CREATE TABLE alert_scored_run (
  rule_id character varying(50) NOT NULL REFERENCES alert_rule (id) ON DELETE CASCADE,
  run_id text NOT NULL,
  finalized_at timestamptz NOT NULL,
  scored_at timestamptz NOT NULL,
  PRIMARY KEY (rule_id, run_id)
);

-- Runs left unscored while their commit metadata is missing. Selection puts
-- runs never attempted ahead of these retries, so a backlog of runs waiting
-- on metadata cannot crowd out newer runs.
CREATE TABLE alert_pending_run (
  rule_id character varying(50) NOT NULL REFERENCES alert_rule (id) ON DELETE CASCADE,
  run_id text NOT NULL,
  attempted_at timestamptz NOT NULL,
  PRIMARY KEY (rule_id, run_id)
);
