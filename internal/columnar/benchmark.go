package columnar

import (
	"context"
	"fmt"
	"slices"

	"go.kenn.io/benchdb/internal/storage"
)

var benchmarkStatuses = []string{"regressed", "improved", "stable", "insufficient"}

func benchmarkRank(status string) (int, error) {
	rank := slices.Index(benchmarkStatuses, status)
	if rank < 0 {
		return 0, fmt.Errorf("unknown benchmark status %q", status)
	}
	return rank, nil
}

const benchmarkScope = `WITH scoped AS MATERIALIZED (
SELECT v.*,cs.name AS case_name,cs.tags AS case_tags,
CASE v.status WHEN 'regressed' THEN 0 WHEN 'improved' THEN 1 WHEN 'stable' THEN 2 ELSE 3 END AS status_rank
FROM benchmark_verdict v JOIN "case" cs ON cs.id=v.case_id
WHERE ($1::VARCHAR IS NULL OR cs.name ILIKE '%'||$1||'%' OR cs.tags ILIKE '%'||$1||'%')
AND ($2::VARCHAR IS NULL OR v.machine_name=$2) AND ($3::VARCHAR IS NULL OR v.repository=$3)
AND ($4::VARCHAR IS NULL OR v.benchmark_id=$4)
AND ($5::TIMESTAMP IS NULL OR v.latest_commit_timestamp>=$5)
AND ($6::TIMESTAMP IS NULL OR v.latest_commit_timestamp<=$6)),
benchmark AS (
SELECT benchmark_id,repository,case_name,case_tags,
CASE WHEN bool_and(unit IS NOT NULL) AND count(DISTINCT unit)=1 THEN min(unit) END AS unit,
CASE WHEN bool_and(unit IS NOT NULL) AND count(DISTINCT unit)=1 THEN min(status_rank) ELSE 3 END AS status_rank,
list(machine_name ORDER BY machine_name) AS machine_names,sum(point_count) AS point_count
FROM scoped GROUP BY benchmark_id,repository,case_name,case_tags)`

// Filter measurement IDs before the final join: inflated dimension estimates
// can otherwise put the entire archive on the hash join's build side.
const benchmarkPageSQL = benchmarkScope + `,
page AS (
SELECT * FROM benchmark WHERE ($7::INTEGER IS NULL OR status_rank=$7)
AND ($8::INTEGER IS NULL OR (status_rank,case_name,benchmark_id)>($8::INTEGER,$9::VARCHAR,$10::VARCHAR))
ORDER BY status_rank,case_name,benchmark_id LIMIT $11),
latest AS (
SELECT s.* FROM scoped s JOIN page p ON p.benchmark_id=s.benchmark_id
QUALIFY row_number() OVER(PARTITION BY s.benchmark_id ORDER BY s.latest_commit_timestamp DESC,s.latest_result_id DESC)=1),
changes AS (
SELECT s.benchmark_id,s.change_percent,s.machine_name FROM scoped s JOIN page p ON p.benchmark_id=s.benchmark_id
QUALIFY row_number() OVER(PARTITION BY s.benchmark_id ORDER BY s.status_rank,abs(s.change_percent) DESC NULLS LAST,s.machine_name)=1),
counts AS (
SELECT br.benchmark_id,count(*) AS point_count FROM benchmark_result br
JOIN "commit" c ON c.id=br.commit_id JOIN hardware hw ON hw.id=br.hardware_id
JOIN scoped s ON s.benchmark_id=br.benchmark_id AND s.machine_name=hw.name
JOIN page p ON p.benchmark_id=br.benchmark_id WHERE ` + historyMembership + `
AND ($5::TIMESTAMP IS NOT NULL OR $6::TIMESTAMP IS NOT NULL)
AND ($5::TIMESTAMP IS NULL OR c."timestamp">=$5) AND ($6::TIMESTAMP IS NULL OR c."timestamp"<=$6)
GROUP BY br.benchmark_id),
previews AS (
SELECT s.benchmark_id,to_json(list(json_object('machine_name',s.machine_name,'points',s.preview_points::JSON)
ORDER BY s.machine_name))::VARCHAR AS preview_tracks FROM scoped s JOIN page p ON p.benchmark_id=s.benchmark_id
GROUP BY s.benchmark_id)
SELECT p.benchmark_id,p.repository,p.case_name,p.case_tags,p.unit,p.status_rank,p.machine_names,
d.change_percent,d.machine_name,
CASE WHEN $5::TIMESTAMP IS NULL AND $6::TIMESTAMP IS NULL THEN p.point_count ELSE coalesce(cnt.point_count,0) END,
pv.preview_tracks,
l.latest_result_id,l.latest_result_timestamp,l.latest_commit_sha,l.latest_commit_timestamp,
br.unit,coalesce(br.data,[]::DOUBLE[])
FROM page p JOIN latest l ON l.benchmark_id=p.benchmark_id
JOIN changes d ON d.benchmark_id=p.benchmark_id JOIN previews pv ON pv.benchmark_id=p.benchmark_id
LEFT JOIN counts cnt ON cnt.benchmark_id=p.benchmark_id
LEFT JOIN (SELECT id,unit,data FROM benchmark_result
WHERE id IN(SELECT latest_result_id FROM latest)) br ON br.id=l.latest_result_id
ORDER BY p.status_rank,p.case_name,p.benchmark_id`

func scanBenchmark(row rowScanner) (out storage.BenchmarkPageRow, err error) {
	var rank int
	var machine string
	err = row.Scan(&out.BenchmarkID, &out.Repository, &out.CaseName, &out.CaseTags,
		&out.Unit, &rank, stringList{&out.MachineNames}, &out.ChangePercent, &machine,
		&out.PointCount, &out.PreviewTracks, &out.LatestResultID, &out.LatestResultTimestamp,
		&out.LatestCommitSha, &out.LatestCommitTimestamp, &out.LatestUnit, denseFloats{&out.LatestData})
	if err != nil {
		return
	}
	if rank < 0 || rank >= len(benchmarkStatuses) {
		return out, fmt.Errorf("unknown benchmark status rank %d", rank)
	}
	out.Status = benchmarkStatuses[rank]
	if out.Unit == nil || *out.Unit == "" {
		out.Unit = nil
		out.ChangePercent = nil
	}
	if out.ChangePercent != nil {
		out.ChangeMachine = &machine
	}
	return
}

const benchmarkPendingSQL = `SELECT EXISTS(SELECT 1 FROM verdict_queue q WHERE
(q.kind='benchmark' AND ($1::VARCHAR IS NULL
OR EXISTS(SELECT 1 FROM benchmark_verdict v WHERE v.benchmark_id=q.key AND v.repository=$1)
OR EXISTS(SELECT 1 FROM benchmark_membership m WHERE m.benchmark_id=q.key AND m.nonerror_count>0 AND m.repository=$1)))
OR (q.kind='commit' AND ($1::VARCHAR IS NULL
OR EXISTS(SELECT 1 FROM "commit" c WHERE c.id=q.key AND c.repository=$1))))`

func (s *Store) SelectBenchmarkList(ctx context.Context, p storage.BenchmarkListParams) (out storage.BenchmarkList, err error) {
	args := []any{p.Q, p.Hardware, p.Repository, p.BenchmarkID, p.ActiveSince, p.ActiveUntil}
	var statusRank, cursorRank *int
	var cursorName, cursorID *string
	if p.Status != nil {
		rank, e := benchmarkRank(*p.Status)
		if e != nil {
			return out, e
		}
		statusRank = &rank
	}
	if p.Cursor != nil {
		rank, e := benchmarkRank(p.Cursor.Status)
		if e != nil {
			return out, e
		}
		cursorRank, cursorName, cursorID = &rank, &p.Cursor.Name, &p.Cursor.ID
	}
	pageArgs := append(slices.Clone(args), statusRank, cursorRank, cursorName, cursorID, int64(p.PageSize)+1)
	err = s.read(ctx, func(q queryer) error {
		rows, e := collectRows(ctx, q, benchmarkPageSQL, pageArgs, scanBenchmark)
		if e != nil {
			return e
		}
		out.More = len(rows) > int(p.PageSize)
		if out.More {
			rows = rows[:p.PageSize]
		}
		out.Rows = rows
		out.StatusCounts = make(map[string]int64, len(benchmarkStatuses))
		for _, status := range benchmarkStatuses {
			out.StatusCounts[status] = 0
		}
		counts, e := q.QueryContext(ctx, benchmarkScope+` SELECT status_rank,count(*) FROM benchmark GROUP BY status_rank`, args...)
		if e != nil {
			return e
		}
		for counts.Next() {
			var rank int
			var count int64
			if e := counts.Scan(&rank, &count); e != nil {
				counts.Close()
				return e
			}
			if rank < 0 || rank >= len(benchmarkStatuses) {
				counts.Close()
				return fmt.Errorf("unknown benchmark status rank %d", rank)
			}
			out.StatusCounts[benchmarkStatuses[rank]] = count
		}
		e = counts.Err()
		if closeErr := counts.Close(); e == nil {
			e = closeErr
		}
		if e != nil {
			return e
		}
		return q.QueryRowContext(ctx, benchmarkPendingSQL, p.Repository).Scan(&out.Pending)
	})
	return
}
