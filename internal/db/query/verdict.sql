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
SET claimed_until = sqlc.arg('lease_until')::timestamptz,
    claim_token = gen_random_uuid()
FROM ready
WHERE q.kind = ready.kind AND q.key = ready.key
RETURNING q.kind, q.key, q.enqueued_at, q.claim_token::uuid AS claim_token;

-- name: LockVerdictClaim :one
-- Holds the claimed queue row for the rest of the transaction. No row means
-- the lease expired and another worker owns the key.
SELECT q.enqueued_at
FROM verdict_queue q
WHERE q.kind = sqlc.arg('kind') AND q.key = sqlc.arg('key') AND q.claim_token = sqlc.arg('claim_token')
FOR UPDATE;

-- name: DeleteVerdictClaim :execrows
DELETE FROM verdict_queue
WHERE kind = sqlc.arg('kind') AND key = sqlc.arg('key')
  AND claim_token = sqlc.arg('claim_token') AND enqueued_at = sqlc.arg('enqueued_at');

-- name: ReleaseVerdictClaim :exec
-- A key re-enqueued while it was processed becomes claimable at once.
UPDATE verdict_queue
SET claimed_until = NULL, claim_token = NULL
WHERE kind = sqlc.arg('kind') AND key = sqlc.arg('key') AND claim_token = sqlc.arg('claim_token');

-- name: EnqueueVerdict :exec
SELECT enqueue_verdict(sqlc.arg('kind')::text, sqlc.arg('key')::text);

-- name: EnqueueMissingRunVerdicts :exec
SELECT enqueue_missing_run_verdicts(sqlc.arg('run_ids')::text[]);

-- name: SelectRepositoryVerdictRuns :many
-- Runs whose attention can change when default-branch history in the
-- repository changes. Default-branch runs never need attention.
SELECT DISTINCT br.run_id
FROM benchmark_result br
LEFT JOIN commit c ON c.id = br.commit_id
WHERE br.commit_repo_url = sqlc.arg('repository')
  AND br."timestamp" >= sqlc.arg('since')::timestamp
  AND (c.id IS NULL OR c.sha IS DISTINCT FROM c.fork_point_sha);

-- name: SelectRunVerdictSubjects :many
-- One subject per repository the run has results in, identified the way the
-- recent-runs list identifies it: by the latest result in that repository.
SELECT DISTINCT ON (br.commit_repo_url)
  br.commit_repo_url,
  c.sha AS commit_sha,
  coalesce(c.sha = c.fork_point_sha, false)::boolean AS default_branch,
  br."timestamp" AS last_result_at,
  br.id AS last_result_id
FROM benchmark_result br
LEFT JOIN commit c ON c.id = br.commit_id
WHERE br.run_id = sqlc.arg('run_id')
ORDER BY br.commit_repo_url, br."timestamp" DESC, br.id DESC;

-- name: UpsertRunVerdict :exec
INSERT INTO run_verdict (run_id, repository, last_result_at, last_result_id, default_branch, needs_attention, attention, computed_at)
VALUES (
  sqlc.arg('run_id'),
  sqlc.arg('repository'),
  sqlc.arg('last_result_at'),
  sqlc.arg('last_result_id'),
  sqlc.arg('default_branch'),
  sqlc.arg('needs_attention'),
  sqlc.narg('attention'),
  clock_timestamp()
)
ON CONFLICT (run_id, repository) DO UPDATE SET
  last_result_at = EXCLUDED.last_result_at,
  last_result_id = EXCLUDED.last_result_id,
  default_branch = EXCLUDED.default_branch,
  needs_attention = EXCLUDED.needs_attention,
  attention = EXCLUDED.attention,
  computed_at = EXCLUDED.computed_at;

-- name: DeleteRunVerdictsExcept :exec
-- Removes verdicts for repositories the run no longer has results in.
DELETE FROM run_verdict
WHERE run_id = sqlc.arg('run_id') AND NOT (repository = ANY(sqlc.arg('repositories')::text[]));

-- name: SelectRunVerdicts :many
-- Every repository's verdict for the runs; the caller picks the repository it
-- shows. A verdict is pending while a recompute that could change it is queued.
SELECT
  v.run_id,
  v.repository,
  v.last_result_at,
  v.default_branch,
  v.needs_attention,
  v.attention,
  (
    EXISTS (SELECT 1 FROM verdict_queue q WHERE q.kind = 'run' AND q.key = v.run_id)
    OR (NOT v.default_branch AND EXISTS (
      SELECT 1 FROM verdict_queue q WHERE q.kind = 'repository' AND q.key = v.repository
    ))
  )::boolean AS pending
FROM run_verdict v
WHERE v.run_id = ANY(sqlc.arg('run_ids')::text[]);

-- name: VerdictsPending :one
-- Whether any queued recompute can change verdicts within the repository
-- filter, so a page can keep refreshing even when it lists no runs.
SELECT EXISTS (
  SELECT 1
  FROM verdict_queue q
  WHERE sqlc.narg('repository')::text IS NULL
    OR (q.kind = 'repository' AND q.key = sqlc.narg('repository')::text)
    OR (q.kind = 'run' AND EXISTS (
      SELECT 1 FROM benchmark_result br
      WHERE br.run_id = q.key AND br.commit_repo_url = sqlc.narg('repository')::text
    ))
)::boolean;

-- name: CountAttentionRuns :one
-- Counts runs the way the list shows them: within one repository, or by the
-- repository of the run's latest result when no repository is selected,
-- breaking timestamp ties by result id as the list does.
SELECT count(*)
FROM run_verdict v
WHERE v.needs_attention
  AND (sqlc.narg('repository')::text IS NULL OR v.repository = sqlc.narg('repository')::text)
  AND (sqlc.narg('repository')::text IS NOT NULL OR NOT EXISTS (
    SELECT 1 FROM run_verdict newer
    WHERE newer.run_id = v.run_id
      AND (newer.last_result_at, newer.last_result_id) > (v.last_result_at, v.last_result_id)
  ));
