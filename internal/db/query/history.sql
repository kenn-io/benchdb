-- name: SelectHistoryForFingerprint :many
-- History membership, ported from history.py:get_history_for_fingerprint.
-- Membership requires non-errored results on the default branch
-- (sha == fork_point_sha), joined to a commit (the INNER JOIN drops results with
-- no commit), and joined to a commit with a non-null timestamp. Also selects
-- change_annotations, from which the service derives begins_distribution_change
-- for the rolling-statistics (zscorestats) computation in internal/stats.
--
-- This query and SelectHistoryForFingerprintsAsOf share one membership
-- definition; both require a non-null commit timestamp because the series is
-- time-ordered by it (a null would sort as the zero time and corrupt the
-- rolling-statistics window).
-- Results annotated exclude_from_history=true remain available for direct
-- comparisons but do not participate in history or its statistical distribution.
SELECT
  br.id,
  br.history_fingerprint,
  br."timestamp",
  br.unit,
  br.mean,
  br.data,
  br.run_tags,
  inf.tags AS info_tags,
  br.change_annotations,
  br.optional_benchmark_info,
  hw.hash AS hardware_hash,
  c.sha AS commit_sha,
  c.repository AS commit_repository,
  c.message AS commit_message,
  c."timestamp" AS commit_timestamp
FROM benchmark_result_local br
JOIN info inf ON inf.id = br.info_id
JOIN hardware hw ON hw.id = br.hardware_id
JOIN commit c ON c.id = br.commit_id
WHERE br.error IS NULL
  AND (br.change_annotations->'exclude_from_history') IS DISTINCT FROM 'true'::jsonb
  AND br.history_fingerprint = $1
  AND c.sha = c.fork_point_sha
  AND c."timestamp" IS NOT NULL
ORDER BY c."timestamp", br.id;

-- name: SelectHistoryForBenchmark :many
-- All directly comparable fingerprint segments for one logical benchmark.
-- Rolling statistics remain fingerprint-local in the service; this query only
-- brings those segments together for fleet presentation.
SELECT
  br.id,
  br.benchmark_id,
  br.history_fingerprint,
  br."timestamp",
  br.unit,
  br.mean,
  br.data,
  br.run_tags,
  i.tags AS info_tags,
  br.change_annotations,
  br.optional_benchmark_info,
  br.case_id,
  cs.name AS case_name,
  cs.tags AS case_tags,
  ctx.tags AS context_tags,
  hw.id AS hardware_id,
  hw.type AS hardware_type,
  hw.name AS hardware_name,
  hw.hash AS hardware_hash,
  br.commit_repo_url,
  c.sha AS commit_sha,
  c.repository AS commit_repository,
  c.message AS commit_message,
  c."timestamp" AS commit_timestamp
FROM benchmark_result_local br
JOIN "case" cs ON cs.id = br.case_id
JOIN context ctx ON ctx.id = br.context_id
JOIN info i ON i.id = br.info_id
JOIN hardware hw ON hw.id = br.hardware_id
JOIN commit c ON c.id = br.commit_id
WHERE br.benchmark_id = sqlc.arg('benchmark_id')
  AND br.error IS NULL
  AND (br.change_annotations->'exclude_from_history') IS DISTINCT FROM 'true'::jsonb
  AND c.sha = c.fork_point_sha
  AND c."timestamp" IS NOT NULL
ORDER BY hw.name, br.history_fingerprint, c."timestamp", br.id;

-- name: SelectHistoryForFingerprintsAsOf :many
-- The baseline-distribution windows for compare and CI reports: the same
-- default-branch membership as SelectHistoryForFingerprint for each requested
-- fingerprint, restricted to commits at or before a cutoff timestamp. The
-- timestamp cutoff approximates git ancestry (Phase-2; true ancestry is Phase
-- 4). Columns match SelectHistoryForFingerprint so both map to
-- storage.HistoryRow.
--
-- Postgres cannot estimate sha = fork_point_sha and, left alone, walks every
-- default-branch commit in every repository before the cutoff. The query starts
-- from the fingerprints' own results and looks each commit up by primary key;
-- OFFSET 0 keeps the planner from flattening the lookup back into a join.
SELECT
  br.id,
  br.history_fingerprint,
  br."timestamp",
  br.unit,
  br.mean,
  br.data,
  br.run_tags,
  inf.tags AS info_tags,
  br.change_annotations,
  br.optional_benchmark_info,
  hw.hash AS hardware_hash,
  c.sha AS commit_sha,
  c.repository AS commit_repository,
  c.message AS commit_message,
  c."timestamp" AS commit_timestamp
FROM benchmark_result_local br
JOIN info inf ON inf.id = br.info_id
JOIN hardware hw ON hw.id = br.hardware_id
JOIN LATERAL (
  SELECT dc.sha, dc.repository, dc.message, dc."timestamp"
  FROM commit dc
  WHERE dc.id = br.commit_id
    AND dc.sha = dc.fork_point_sha
    AND dc."timestamp" IS NOT NULL
    AND dc."timestamp" <= sqlc.arg('as_of')
  OFFSET 0
) c ON true
WHERE br.history_fingerprint = ANY(sqlc.arg('history_fingerprints')::text[])
  AND br.error IS NULL
  AND (br.change_annotations->'exclude_from_history') IS DISTINCT FROM 'true'::jsonb
ORDER BY br.history_fingerprint, c."timestamp", br.id;
