-- name: GetCommitByShaRepo :one
SELECT id FROM commit WHERE sha = $1 AND repository = $2;

-- name: InsertCommit :one
INSERT INTO commit (
  id, sha, parent, repository, message, author_name,
  author_login, author_avatar, timestamp, branch, fork_point_sha
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (sha, repository) DO NOTHING
RETURNING id;

-- name: GetLatestCommitTimestampOnBranch :one
-- The ancestry backfill's "last tracked commit" lookup: the newest
-- default-branch commit strictly before the submitted commit's timestamp
-- (legacy backfill_default_branch_commits, commit.py:488-497).
SELECT timestamp FROM commit
WHERE repository = $1 AND branch = $2
  AND timestamp IS NOT NULL AND timestamp < $3
ORDER BY timestamp DESC
LIMIT 1;

-- name: SelectRepositories :many
-- Repositories that still have benchmark results, most recently active first.
-- Commit rows outlive deleted results, so a commit counts only while a result
-- references it. Results submitted without a commit carry the repository only
-- in commit_repo_url. Both branches use benchmark_result_commit_id_index, so
-- the query never scans the whole results table.
SELECT repository
FROM (
  SELECT c.repository, max(c."timestamp") AS last_activity
  FROM commit c
  WHERE c.repository <> ''
    AND EXISTS (SELECT 1 FROM benchmark_result br WHERE br.commit_id = c.id)
  GROUP BY c.repository
  UNION ALL
  SELECT br.commit_repo_url, max(br."timestamp")
  FROM benchmark_result br
  WHERE br.commit_id IS NULL
    AND br.commit_repo_url <> ''
  GROUP BY br.commit_repo_url
) active
GROUP BY repository
ORDER BY max(last_activity) DESC NULLS LAST, repository ASC;

-- name: SelectUnknownCommitRepairCandidates :many
SELECT id, sha, repository
FROM commit
WHERE timestamp IS NULL
  AND fork_point_sha IS NULL
  AND sha <> ''
  AND repository <> ''
  AND (sqlc.narg('repository')::text IS NULL OR repository = sqlc.narg('repository')::text)
  AND (
    sqlc.narg('after_repository')::text IS NULL
    OR (repository, sha) > (sqlc.narg('after_repository')::text, sqlc.narg('after_sha')::text)
  )
ORDER BY repository ASC, sha ASC
LIMIT sqlc.arg('limit_plus_one')::integer;

-- name: UpdateUnknownCommit :execrows
UPDATE commit
SET parent = sqlc.arg('parent'),
    message = sqlc.arg('message'),
    author_name = sqlc.arg('author_name'),
    author_login = sqlc.arg('author_login'),
    author_avatar = sqlc.arg('author_avatar'),
    timestamp = sqlc.arg('timestamp'),
    branch = sqlc.arg('branch'),
    fork_point_sha = sqlc.arg('fork_point_sha')
WHERE id = sqlc.arg('id')
  AND timestamp IS NULL
  AND fork_point_sha IS NULL;
