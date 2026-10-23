-- An alert rule opens only when a benchmark regresses in confirm_runs
-- consecutive scored runs, so a one-run blip does not alert. The default of
-- two applies to existing rules. regression_streaks maps each benchmark
-- series (history fingerprint) that regressed in the latest scored run to how
-- many scored runs in a row it has regressed.
ALTER TABLE alert_rule
  ADD COLUMN confirm_runs integer NOT NULL DEFAULT 2 CHECK (confirm_runs BETWEEN 1 AND 10),
  ADD COLUMN regression_streaks jsonb NOT NULL DEFAULT '{}'::jsonb;
