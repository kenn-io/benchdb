-- name: SelectBenchmarkPage :many
-- One row per logical benchmark (case and repository), built from its stored
-- machine tracks. The machine filter keeps that machine's track, and the
-- active window keeps tracks whose latest default-branch commit falls inside
-- it. A benchmark's status rank (0 regressed, 1 improved, 2 stable,
-- 3 insufficient) is the worst across its tracks, or insufficient when the
-- tracks do not share one unit. Pages run in status order, regressions first,
-- then by name. Point counts cover the tracks' history points inside the
-- active window, and previews hold each track's stored latest points. The
-- change is the stored change of the track that sets the benchmark's status,
-- the largest one when several tracks share that status; callers drop it when
-- the tracks do not share one unit.
-- CountBenchmarkStatuses applies the same scope.
WITH scoped AS (
  SELECT
    v.benchmark_id,
    v.machine_name,
    v.repository,
    v.point_count,
    v.preview_points,
    v.unit,
    v.change_percent,
    v.latest_result_id,
    v.latest_result_timestamp,
    v.latest_commit_sha,
    v.latest_commit_timestamp,
    cs.name AS case_name,
    cs.tags AS case_tags,
    CASE v.status WHEN 'regressed' THEN 0 WHEN 'improved' THEN 1 WHEN 'stable' THEN 2 ELSE 3 END AS status_rank
  FROM benchmark_verdict v
  JOIN "case" cs ON cs.id = v.case_id
  WHERE (sqlc.narg('q')::text IS NULL OR cs.name ILIKE '%' || sqlc.narg('q')::text || '%' OR cs.tags::text ILIKE '%' || sqlc.narg('q')::text || '%')
    AND (sqlc.narg('hardware')::text IS NULL OR v.machine_name = sqlc.narg('hardware')::text)
    AND (sqlc.narg('repository')::text IS NULL OR v.repository = sqlc.narg('repository')::text)
    AND (sqlc.narg('benchmark_id')::text IS NULL OR v.benchmark_id = sqlc.narg('benchmark_id')::text)
    AND (sqlc.narg('active_since')::timestamp IS NULL OR v.latest_commit_timestamp >= sqlc.narg('active_since')::timestamp)
    AND (sqlc.narg('active_until')::timestamp IS NULL OR v.latest_commit_timestamp <= sqlc.narg('active_until')::timestamp)
),
benchmark AS (
  SELECT
    s.benchmark_id,
    s.repository,
    s.case_name,
    s.case_tags,
    -- Empty when the tracks do not share one unit.
    coalesce(CASE WHEN bool_and(s.unit IS NOT NULL) AND count(DISTINCT s.unit) = 1 THEN min(s.unit) END, '')::text AS unit,
    (CASE WHEN bool_and(s.unit IS NOT NULL) AND count(DISTINCT s.unit) = 1 THEN min(s.status_rank) ELSE 3 END)::integer AS status_rank,
    array_agg(s.machine_name ORDER BY s.machine_name)::text[] AS machine_names
  FROM scoped s
  GROUP BY s.benchmark_id, s.repository, s.case_name, s.case_tags
),
page AS (
  SELECT b.*
  FROM benchmark b
  WHERE (sqlc.narg('status_rank')::integer IS NULL OR b.status_rank = sqlc.narg('status_rank')::integer)
    AND (
      sqlc.narg('cursor_rank')::integer IS NULL
      OR (b.status_rank, b.case_name, b.benchmark_id)
         > (sqlc.narg('cursor_rank')::integer, sqlc.narg('cursor_name')::text, sqlc.narg('cursor_id')::text)
    )
  ORDER BY b.status_rank, b.case_name, b.benchmark_id
  LIMIT sqlc.arg('page_size')
),
latest AS (
  SELECT DISTINCT ON (s.benchmark_id)
    s.benchmark_id,
    s.latest_result_id,
    s.latest_result_timestamp,
    s.latest_commit_sha,
    s.latest_commit_timestamp
  FROM scoped s
  JOIN page p ON p.benchmark_id = s.benchmark_id
  ORDER BY s.benchmark_id, s.latest_commit_timestamp DESC, s.latest_result_id DESC
)
SELECT
  p.benchmark_id,
  p.repository,
  p.case_name,
  p.case_tags,
  p.unit,
  p.status_rank,
  p.machine_names,
  d.change_percent,
  d.machine_name AS change_machine,
  (
    SELECT coalesce(sum(CASE
      WHEN sqlc.narg('active_since')::timestamp IS NULL AND sqlc.narg('active_until')::timestamp IS NULL
        THEN s.point_count
      ELSE (
        SELECT count(*)
        FROM benchmark_result_local br
        JOIN commit c ON c.id = br.commit_id
        JOIN hardware h ON h.id = br.hardware_id
        WHERE br.benchmark_id = s.benchmark_id AND h.name = s.machine_name
          AND br.error IS NULL AND c.sha = c.fork_point_sha
          AND c."timestamp" IS NOT NULL
          AND (br.change_annotations->'exclude_from_history') IS DISTINCT FROM 'true'::jsonb
          AND (sqlc.narg('active_since')::timestamp IS NULL OR c."timestamp" >= sqlc.narg('active_since')::timestamp)
          AND (sqlc.narg('active_until')::timestamp IS NULL OR c."timestamp" <= sqlc.narg('active_until')::timestamp)
      ) END), 0)
    FROM scoped s
    WHERE s.benchmark_id = p.benchmark_id
  )::bigint AS point_count,
  (
    SELECT jsonb_agg(jsonb_build_object('machine_name', s.machine_name, 'points', s.preview_points)
                     ORDER BY s.machine_name)
    FROM scoped s
    WHERE s.benchmark_id = p.benchmark_id
  )::jsonb AS preview_tracks,
  l.latest_result_id,
  l.latest_result_timestamp,
  l.latest_commit_sha,
  l.latest_commit_timestamp,
  -- A deleted latest result leaves its benchmark queued for recompute; the
  -- benchmark stays listed without a latest value until then.
  br.unit AS latest_unit,
  coalesce(br.data, ARRAY[]::numeric[])::numeric[] AS latest_data
FROM page p
JOIN latest l ON l.benchmark_id = p.benchmark_id
LEFT JOIN LATERAL (
  SELECT s.change_percent, s.machine_name
  FROM scoped s
  WHERE s.benchmark_id = p.benchmark_id
  ORDER BY s.status_rank, abs(s.change_percent) DESC NULLS LAST, s.machine_name
  LIMIT 1
) d ON true
LEFT JOIN benchmark_result_local br ON br.id = l.latest_result_id
ORDER BY p.status_rank, p.case_name, p.benchmark_id;

-- name: CountBenchmarkStatuses :many
-- Benchmarks per status rank within SelectBenchmarkPage's scope, before its
-- status filter and cursor.
WITH scoped AS (
  SELECT
    v.benchmark_id,
    v.unit,
    CASE v.status WHEN 'regressed' THEN 0 WHEN 'improved' THEN 1 WHEN 'stable' THEN 2 ELSE 3 END AS status_rank
  FROM benchmark_verdict v
  JOIN "case" cs ON cs.id = v.case_id
  WHERE (sqlc.narg('q')::text IS NULL OR cs.name ILIKE '%' || sqlc.narg('q')::text || '%' OR cs.tags::text ILIKE '%' || sqlc.narg('q')::text || '%')
    AND (sqlc.narg('hardware')::text IS NULL OR v.machine_name = sqlc.narg('hardware')::text)
    AND (sqlc.narg('repository')::text IS NULL OR v.repository = sqlc.narg('repository')::text)
    AND (sqlc.narg('benchmark_id')::text IS NULL OR v.benchmark_id = sqlc.narg('benchmark_id')::text)
    AND (sqlc.narg('active_since')::timestamp IS NULL OR v.latest_commit_timestamp >= sqlc.narg('active_since')::timestamp)
    AND (sqlc.narg('active_until')::timestamp IS NULL OR v.latest_commit_timestamp <= sqlc.narg('active_until')::timestamp)
),
benchmark AS (
  SELECT
    (CASE WHEN bool_and(s.unit IS NOT NULL) AND count(DISTINCT s.unit) = 1 THEN min(s.status_rank) ELSE 3 END)::integer AS status_rank
  FROM scoped s
  GROUP BY s.benchmark_id
)
SELECT b.status_rank, count(*)::bigint AS benchmarks
FROM benchmark b
GROUP BY b.status_rank
ORDER BY b.status_rank;

-- name: SelectBenchmarkMachines :many
-- Machines with a stored benchmark track, by name, optionally within one
-- repository: exactly the machines the benchmark list can filter to.
SELECT DISTINCT v.machine_name
FROM benchmark_verdict v
WHERE sqlc.narg('repository')::text IS NULL OR v.repository = sqlc.narg('repository')::text
ORDER BY v.machine_name;
