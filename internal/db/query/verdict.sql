-- name: ClaimVerdictQueue :many
-- Claim settled keys with a lease so concurrent servers never process the same
-- key at once and a crashed worker's keys become claimable again.
WITH ready AS (
  SELECT q.kind, q.key
  FROM verdict_queue q
  WHERE q.enqueued_at <= sqlc.arg('settled_before')::timestamptz
    AND (q.claimed_until IS NULL OR q.claimed_until < clock_timestamp())
  ORDER BY q.enqueued_at
  LIMIT sqlc.arg('batch_size')
  FOR UPDATE SKIP LOCKED
)
UPDATE verdict_queue AS q
SET claimed_until = sqlc.arg('lease_until')::timestamptz
FROM ready
WHERE q.kind = ready.kind AND q.key = ready.key
RETURNING q.kind, q.key, q.enqueued_at;

-- name: CompleteVerdictQueue :exec
-- A key re-enqueued while it was being processed keeps its newer row.
DELETE FROM verdict_queue
WHERE kind = sqlc.arg('kind') AND key = sqlc.arg('key') AND enqueued_at = sqlc.arg('enqueued_at');

-- name: EnqueueVerdict :exec
SELECT enqueue_verdict(sqlc.arg('kind')::text, sqlc.arg('key')::text);

-- name: EnqueueMissingRunVerdicts :exec
INSERT INTO verdict_queue (kind, key)
SELECT 'run', ids.run_id
FROM unnest(sqlc.arg('run_ids')::text[]) AS ids(run_id)
WHERE NOT EXISTS (SELECT 1 FROM run_verdict v WHERE v.run_id = ids.run_id)
ON CONFLICT DO NOTHING;

-- name: SelectRepositoryVerdictRuns :many
-- Runs whose attention can change when default-branch history in the
-- repository changes. Default-branch runs never need attention.
SELECT DISTINCT br.run_id
FROM benchmark_result br
LEFT JOIN commit c ON c.id = br.commit_id
WHERE br.commit_repo_url = sqlc.arg('repository')
  AND br."timestamp" >= sqlc.arg('since')::timestamp
  AND (c.id IS NULL OR c.sha IS DISTINCT FROM c.fork_point_sha);

-- name: GetRunVerdictSubject :one
-- The run's identity as the recent-runs list reports it: its latest result's
-- repository and commit.
SELECT
  latest.commit_repo_url,
  c.sha AS commit_sha,
  span.last_result_at::timestamp AS last_result_at
FROM (
  SELECT max(br."timestamp") AS last_result_at
  FROM benchmark_result br
  WHERE br.run_id = sqlc.arg('run_id')
) span
JOIN LATERAL (
  SELECT br.commit_repo_url, br.commit_id
  FROM benchmark_result br
  WHERE br.run_id = sqlc.arg('run_id')
  ORDER BY br."timestamp" DESC, br.id DESC
  LIMIT 1
) latest ON true
LEFT JOIN commit c ON c.id = latest.commit_id;

-- name: UpsertRunVerdict :exec
INSERT INTO run_verdict (run_id, repository, last_result_at, needs_attention, attention, computed_at)
VALUES (
  sqlc.arg('run_id'),
  sqlc.arg('repository'),
  sqlc.arg('last_result_at'),
  sqlc.arg('needs_attention'),
  sqlc.narg('attention'),
  clock_timestamp()
)
ON CONFLICT (run_id) DO UPDATE SET
  repository = EXCLUDED.repository,
  last_result_at = EXCLUDED.last_result_at,
  needs_attention = EXCLUDED.needs_attention,
  attention = EXCLUDED.attention,
  computed_at = EXCLUDED.computed_at;

-- name: DeleteRunVerdict :exec
DELETE FROM run_verdict WHERE run_id = sqlc.arg('run_id');

-- name: SelectRunVerdicts :many
SELECT v.run_id, v.repository, v.last_result_at, v.needs_attention, v.attention
FROM run_verdict v
WHERE v.run_id = ANY(sqlc.arg('run_ids')::text[]);

-- name: CountAttentionRuns :one
SELECT count(*)
FROM run_verdict v
WHERE v.needs_attention
  AND (sqlc.narg('repository')::text IS NULL OR v.repository = sqlc.narg('repository')::text);
