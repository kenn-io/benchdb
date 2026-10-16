-- name: InsertAlertRule :one
INSERT INTO alert_rule (
  id, user_id, name, repository, baseline, threshold, threshold_z,
  run_reason, enabled, state, created_at, updated_at
)
VALUES (
  $1, $2, $3, $4, $5, $6, $7,
  $8, $9, 'inactive', $10, $10
)
RETURNING *;

-- name: GetAlertRule :one
SELECT * FROM alert_rule WHERE id = $1;

-- name: ListAlertRulesByUser :many
SELECT * FROM alert_rule
WHERE user_id = $1
ORDER BY created_at DESC, id DESC;

-- name: ListEnabledAlertRules :many
SELECT * FROM alert_rule
WHERE enabled = true
ORDER BY created_at ASC, id ASC;

-- name: UpdateAlertRule :one
UPDATE alert_rule
SET
  name = $3,
  repository = $4,
  baseline = $5,
  threshold = $6,
  threshold_z = $7,
  run_reason = $8,
  enabled = $9,
  updated_at = $10,
  state = CASE WHEN sqlc.arg('reset_evaluation')::boolean THEN 'inactive' ELSE state END,
  last_evaluated_at = CASE WHEN sqlc.arg('reset_evaluation')::boolean THEN NULL ELSE last_evaluated_at END,
  report_start_at = CASE WHEN sqlc.arg('reset_evaluation')::boolean THEN NULL ELSE report_start_at END
WHERE id = $1 AND user_id = $2
RETURNING *;

-- name: DeleteAlertRule :execrows
DELETE FROM alert_rule WHERE id = $1 AND user_id = $2;

-- name: UpdateAlertRuleEvaluation :one
UPDATE alert_rule
SET state = $2, last_evaluated_at = $3, updated_at = $3
WHERE id = $1
RETURNING *;

-- name: TouchAlertRuleEvaluation :one
UPDATE alert_rule
SET
  last_evaluated_at = sqlc.arg('evaluated_at'),
  updated_at = sqlc.arg('evaluated_at')
WHERE alert_rule.id = sqlc.arg('id')
  AND alert_rule.state = sqlc.arg('state')
  AND alert_rule.enabled = true
  AND alert_rule.repository = sqlc.arg('repository')
  AND alert_rule.baseline = sqlc.arg('baseline')
  AND alert_rule.threshold = sqlc.arg('threshold')
  AND alert_rule.threshold_z = sqlc.arg('threshold_z')
  AND alert_rule.run_reason IS NOT DISTINCT FROM sqlc.narg('run_reason')
RETURNING *;

-- name: InsertAlertEvent :one
INSERT INTO alert_event (
  id, rule_id, repository, kind, status, status_reason, run_id, commit_sha,
  report_url, summary, created_at
)
SELECT
  sqlc.arg('id'), r.id, r.repository, sqlc.arg('kind'), sqlc.arg('status'),
  sqlc.arg('status_reason'), sqlc.narg('run_id'), sqlc.narg('commit_sha'),
  sqlc.arg('report_url'), sqlc.arg('summary'), sqlc.arg('created_at')
FROM alert_rule r
WHERE r.id = sqlc.arg('rule_id')
RETURNING *;

-- name: TransitionAlertRule :one
WITH updated_rule AS (
  UPDATE alert_rule
  SET
    state = sqlc.arg('to_state'),
    last_evaluated_at = sqlc.arg('evaluated_at'),
    updated_at = sqlc.arg('evaluated_at')
  WHERE alert_rule.id = sqlc.arg('id')
    AND state = sqlc.arg('from_state')
    AND alert_rule.enabled = true
    AND alert_rule.repository = sqlc.arg('repository')
    AND alert_rule.baseline = sqlc.arg('baseline')
    AND alert_rule.threshold = sqlc.arg('threshold')
    AND alert_rule.threshold_z = sqlc.arg('threshold_z')
    AND alert_rule.run_reason IS NOT DISTINCT FROM sqlc.narg('run_reason')
  RETURNING *
),
inserted_event AS (
  INSERT INTO alert_event (
    id, rule_id, repository, kind, status, status_reason, run_id, commit_sha,
    report_url, summary, created_at
  )
  SELECT
    sqlc.arg('event_id'), updated_rule.id, updated_rule.repository,
    sqlc.arg('kind'), sqlc.arg('status'), sqlc.arg('status_reason'),
    sqlc.narg('run_id'), sqlc.narg('commit_sha'), sqlc.arg('report_url'),
    sqlc.arg('summary'), sqlc.arg('evaluated_at')
  FROM updated_rule
  RETURNING *
)
SELECT
  updated_rule.id AS rule_id,
  updated_rule.user_id AS rule_user_id,
  updated_rule.name AS rule_name,
  updated_rule.repository AS rule_repository,
  updated_rule.baseline AS rule_baseline,
  updated_rule.threshold AS rule_threshold,
  updated_rule.threshold_z AS rule_threshold_z,
  updated_rule.run_reason AS rule_run_reason,
  updated_rule.enabled AS rule_enabled,
  updated_rule.state AS rule_state,
  updated_rule.created_at AS rule_created_at,
  updated_rule.updated_at AS rule_updated_at,
  updated_rule.last_evaluated_at AS rule_last_evaluated_at,
  updated_rule.report_start_at AS rule_report_start_at,
  inserted_event.id AS event_id,
  inserted_event.rule_id AS event_rule_id,
  inserted_event.kind AS event_kind,
  inserted_event.status AS event_status,
  inserted_event.status_reason AS event_status_reason,
  inserted_event.run_id AS event_run_id,
  inserted_event.commit_sha AS event_commit_sha,
  inserted_event.repository AS event_repository,
  inserted_event.report_url AS event_report_url,
  inserted_event.summary AS event_summary,
  inserted_event.created_at AS event_created_at
FROM updated_rule
CROSS JOIN inserted_event;

-- name: ListAlertEventsByRule :many
SELECT * FROM alert_event
WHERE rule_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: StartAlertRule :one
-- Starts a rule at the finalize time of its newest finalized run, before it
-- scores anything, so history is not replayed and a failed first score is
-- retried rather than passed over by a newer run. A rule with no finalized
-- run stays unstarted.
UPDATE alert_rule r
SET report_start_at = (
  SELECT max(cr.evaluated_at)
  FROM ci_run_report cr
  WHERE cr.report->>'repository' = r.repository
    AND cr.report->>'commit_sha' IS NOT NULL
    AND (r.run_reason IS NULL OR EXISTS (
      SELECT 1 FROM jsonb_array_elements(cr.report->'runs') AS run
      WHERE run->>'run_reason' = r.run_reason
    ))
)
WHERE r.id = $1 AND r.report_start_at IS NULL
RETURNING *;

-- name: SelectAlertRunsToScore :many
-- Finalized runs a rule has not scored, finalized at or after its start,
-- oldest first. A run counts when any of its saved runs has the rule's run
-- reason. The ledger, not a timestamp cursor, decides what is left, so a
-- report that becomes visible late is still scored.
SELECT
  cr.run_id,
  (cr.report->>'commit_sha')::text AS commit_sha,
  cr.evaluated_at
FROM alert_rule r
JOIN ci_run_report cr ON cr.report->>'repository' = r.repository
WHERE r.id = sqlc.arg('rule_id')
  AND r.report_start_at IS NOT NULL
  AND cr.evaluated_at >= r.report_start_at
  AND cr.report->>'commit_sha' IS NOT NULL
  AND (r.run_reason IS NULL OR EXISTS (
    SELECT 1 FROM jsonb_array_elements(cr.report->'runs') AS run
    WHERE run->>'run_reason' = r.run_reason
  ))
  AND NOT EXISTS (
    SELECT 1 FROM alert_scored_run s WHERE s.rule_id = r.id AND s.run_id = cr.run_id
  )
ORDER BY cr.evaluated_at, cr.run_id
LIMIT sqlc.arg('row_limit')::int;

-- name: InsertAlertScoredRun :exec
INSERT INTO alert_scored_run (rule_id, run_id, scored_at)
VALUES ($1, $2, $3)
ON CONFLICT (rule_id, run_id) DO NOTHING;

-- name: DeleteAlertScoredRuns :exec
DELETE FROM alert_scored_run WHERE rule_id = $1;

-- name: SelectAlertEventsWithoutDelivery :many
SELECT e.* FROM alert_event e
WHERE NOT EXISTS (
  SELECT 1
  FROM alert_delivery d
  WHERE d.event_id = e.id
    AND d.channel = sqlc.arg('channel')
    AND d.target = sqlc.arg('target')
)
  AND (sqlc.arg('channel') NOT IN ('github-check', 'github-comment') OR e.repository = sqlc.arg('target'))
ORDER BY e.created_at ASC, e.id ASC
LIMIT sqlc.arg('limit')::int;

-- name: InsertAlertDelivery :one
INSERT INTO alert_delivery (
  id, event_id, channel, target, status, attempt_count, created_at, updated_at
)
VALUES (
  $1, $2, $3, $4, 'pending', 0, $5, $5
)
ON CONFLICT (event_id, channel, target) DO NOTHING
RETURNING *;

-- name: ClaimPendingAlertDeliveries :many
-- Atomically claim due deliveries so overlapping deliver runs never select the
-- same row. FOR UPDATE SKIP LOCKED takes the eligible rows under a row lock and
-- skips any a concurrent run already holds; the UPDATE leases each claimed row
-- by pushing next_attempt_at to lease_until, so it is hidden from other runs
-- (and re-eligible after the lease if the sender crashes before recording an
-- outcome). attempt_count is incremented here, at claim time, so completion is
-- recorded without bumping it again.
WITH eligible AS (
  SELECT d.id
  FROM alert_delivery d
  WHERE d.channel = sqlc.arg('channel')
    AND d.target = sqlc.arg('target')
    AND d.status <> 'delivered'
    AND (d.next_attempt_at IS NULL OR d.next_attempt_at <= sqlc.arg('now'))
  ORDER BY d.created_at ASC, d.id ASC
  LIMIT sqlc.arg('limit')::int
  FOR UPDATE SKIP LOCKED
),
claimed AS (
  UPDATE alert_delivery d
  SET
    attempt_count = d.attempt_count + 1,
    last_attempt_at = sqlc.arg('now'),
    next_attempt_at = sqlc.arg('lease_until'),
    updated_at = sqlc.arg('now')
  FROM eligible
  WHERE d.id = eligible.id
  RETURNING
    d.id, d.event_id, d.channel, d.target, d.status, d.attempt_count,
    d.last_attempt_at, d.next_attempt_at, d.delivered_at, d.last_error,
    d.created_at, d.updated_at
)
SELECT
  claimed.id AS delivery_id,
  claimed.event_id AS delivery_event_id,
  claimed.channel AS delivery_channel,
  claimed.target AS delivery_target,
  claimed.status AS delivery_status,
  claimed.attempt_count AS delivery_attempt_count,
  claimed.last_attempt_at AS delivery_last_attempt_at,
  claimed.next_attempt_at AS delivery_next_attempt_at,
  claimed.delivered_at AS delivery_delivered_at,
  claimed.last_error AS delivery_last_error,
  claimed.created_at AS delivery_created_at,
  claimed.updated_at AS delivery_updated_at,
  e.id AS event_id,
  e.rule_id AS event_rule_id,
  e.kind AS event_kind,
  e.status AS event_status,
  e.status_reason AS event_status_reason,
  e.run_id AS event_run_id,
  e.commit_sha AS event_commit_sha,
  e.repository AS event_repository,
  e.report_url AS event_report_url,
  e.summary AS event_summary,
  e.created_at AS event_created_at
FROM claimed
JOIN alert_event e ON e.id = claimed.event_id
ORDER BY claimed.created_at ASC, claimed.id ASC;

-- name: MarkAlertDeliveryDelivered :one
UPDATE alert_delivery
SET
  status = 'delivered',
  last_attempt_at = sqlc.arg('attempted_at'),
  next_attempt_at = NULL,
  delivered_at = sqlc.arg('attempted_at'),
  last_error = NULL,
  updated_at = sqlc.arg('attempted_at')
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: MarkAlertDeliveryFailed :one
UPDATE alert_delivery
SET
  status = 'failed',
  last_attempt_at = sqlc.arg('attempted_at'),
  next_attempt_at = sqlc.arg('next_attempt_at'),
  last_error = sqlc.arg('last_error'),
  updated_at = sqlc.arg('attempted_at')
WHERE id = sqlc.arg('id')
  AND status <> 'delivered'
RETURNING *;
