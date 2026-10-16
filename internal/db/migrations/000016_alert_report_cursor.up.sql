-- Alert rules score every finalized run for their repository, not only the
-- newest one. The cursor is the last finalized report a rule has scored.
ALTER TABLE alert_rule
  ADD COLUMN report_cursor_at timestamptz,
  ADD COLUMN report_cursor_run_id text;
