package columnar

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/benchdb/internal/storage"
)

func (s *Store) SelectBenchmarkResults(ctx context.Context, p storage.ListResultsParams) ([]storage.ResultListRow, error) {
	return readRows(ctx, s, `SELECT br.id,br.run_id,br.run_reason,br.run_tags,br.batch_id,br."timestamp",
br.unit,br.data,br.error,br.history_fingerprint,cs.name,cs.tags,c.sha,c.repository,c.message,
c.author_name,c.author_login,c.author_avatar,c."timestamp",coalesce(c.sha=c.fork_point_sha,false)
FROM benchmark_result br JOIN "case" cs ON cs.id=br.case_id LEFT JOIN "commit" c ON c.id=br.commit_id
WHERE ($1::VARCHAR IS NULL OR br.run_id=$1) AND ($2::VARCHAR IS NULL OR br.batch_id=$2)
AND ($3::VARCHAR IS NULL OR br.run_reason=$3) AND ($4::TIMESTAMP IS NULL OR br."timestamp">=$4)
AND ($5::TIMESTAMP IS NULL OR br."timestamp"<=$5) AND ($6::VARCHAR IS NULL OR br.id<$6)
ORDER BY br.id DESC LIMIT $7`, []any{p.RunID, p.BatchID, p.RunReason, p.Earliest, p.Latest, p.Cursor, p.PageSize}, func(row rowScanner) (out storage.ResultListRow, err error) {
		err = row.Scan(&out.ID, &out.RunID, &out.RunReason, &out.RunTags, &out.BatchID, &out.Timestamp,
			&out.Unit, nullableFloats{&out.Data}, &out.Error, &out.HistoryFingerprint, &out.CaseName,
			&out.CaseTags, &out.CommitSha, &out.CommitRepository, &out.CommitMessage, &out.CommitAuthorName,
			&out.CommitAuthorLogin, &out.CommitAuthorAvatar, &out.CommitTimestamp, &out.CommitIsDefaultBranch)
		return
	})
}

func (s *Store) SelectRepositories(ctx context.Context) ([]storage.RepositoryRow, error) {
	return readRows(ctx, s, `SELECT repository FROM (
SELECT c.repository,max(c."timestamp") AS last_activity FROM "commit" c
WHERE c.repository<>'' AND EXISTS(SELECT 1 FROM benchmark_result br WHERE br.commit_id=c.id)
GROUP BY c.repository
UNION ALL
SELECT br.commit_repo_url,max(br."timestamp") FROM benchmark_result br
WHERE br.commit_id IS NULL AND br.commit_repo_url<>'' GROUP BY br.commit_repo_url
) active GROUP BY repository ORDER BY max(last_activity) DESC NULLS LAST,repository`, nil, func(row rowScanner) (out storage.RepositoryRow, err error) {
		err = row.Scan(&out.Repository)
		return
	})
}

func (s *Store) SelectRecentRuns(ctx context.Context, p storage.RecentRunsParams) ([]storage.RecentRunRow, error) {
	if p.Search == "" && !p.NeedsAttention {
		return s.recentRunPage(ctx, p)
	}
	pattern := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(p.Search) + "%"
	// A shared CTE materializes all metadata before selecting a page. Repeated
	// subqueries let each scan project and filter only what its consumer needs.
	scoped := `SELECT id,run_id,"timestamp",error,history_fingerprint,batch_id,hardware_id,
run_reason,run_tags,commit_repo_url,commit_id
FROM benchmark_result WHERE $1::VARCHAR IS NULL OR commit_repo_url=$1`
	return readRows(ctx, s, `WITH matching_runs AS (
SELECT DISTINCT br.run_id FROM (`+scoped+`) br JOIN "commit" c ON c.id=br.commit_id
WHERE $2<>'' AND (rtrim(c.repository,'/')||'/commit/'||c.sha) ILIKE $6 ESCAPE '!'),
candidate_runs AS (
SELECT br.run_id,max(br."timestamp") AS last_result_at FROM (`+scoped+`) br
WHERE NOT $3 AND ($2='' OR br.run_id IN (SELECT run_id FROM matching_runs)) GROUP BY br.run_id
UNION ALL
SELECT v.run_id,v.last_result_at FROM run_verdict v WHERE $3 AND v.needs_attention
AND ($1::VARCHAR IS NULL OR v.repository=$1)
AND ($1::VARCHAR IS NOT NULL OR NOT EXISTS(SELECT 1 FROM run_verdict newer WHERE newer.run_id=v.run_id
AND (newer.last_result_at,newer.last_result_id)>(v.last_result_at,v.last_result_id)))
AND ($2='' OR v.run_id IN (SELECT run_id FROM matching_runs))),
selected AS (SELECT run_id FROM candidate_runs ORDER BY last_result_at DESC,run_id DESC LIMIT $5 OFFSET $4),
`+fmt.Sprintf(recentSummarySQL, `SELECT br.* FROM (`+scoped+`) br JOIN selected s ON s.run_id=br.run_id`), []any{p.Repository, p.Search, p.NeedsAttention, p.Offset, p.PageSize, pattern}, scanRecentRun)
}

// The source is already restricted to the selected runs. Keep exact counts,
// distinct values, and latest metadata together across all storage tiers.
const recentSummarySQL = `aggregated AS (
SELECT br.run_id,min(br."timestamp") AS first_result_at,max(br."timestamp") AS last_result_at,
count(*) AS result_count,count(*) FILTER(WHERE br.error IS NOT NULL) AS error_count,
count(DISTINCT br.history_fingerprint) AS series_count,count(DISTINCT br.batch_id) AS batch_count,
list(DISTINCT h.name ORDER BY h.name) AS machine_names,
arg_max(struct_pack(id:=br.id,run_reason:=br.run_reason,run_tags:=br.run_tags,batch_id:=br.batch_id,
commit_repo_url:=br.commit_repo_url,commit_id:=br.commit_id),(br."timestamp",br.id)) AS latest
FROM (%s) br JOIN hardware h ON h.id=br.hardware_id GROUP BY br.run_id)
SELECT a.run_id,a.first_result_at,a.last_result_at,a.result_count,a.error_count,a.series_count,a.batch_count,
a.machine_names,a.latest.id,a.latest.run_reason,a.latest.run_tags,a.latest.batch_id,a.latest.commit_repo_url,
c.sha,c.repository,c.message,c.author_name,c.author_login,c.author_avatar,c."timestamp",coalesce(c.sha=c.fork_point_sha,false)
FROM aggregated a LEFT JOIN "commit" c ON c.id=a.latest.commit_id
ORDER BY a.last_result_at DESC,a.run_id DESC`

func scanRecentRun(row rowScanner) (out storage.RecentRunRow, err error) {
	err = row.Scan(&out.RunID, &out.FirstResultAt, &out.LastResultAt, &out.ResultCount, &out.ErrorCount,
		&out.SeriesCount, &out.BatchCount, stringList{&out.MachineNames}, &out.LatestResultID,
		&out.RunReason, &out.RunTags, &out.LatestBatchID, &out.Repository, &out.CommitSha,
		&out.CommitRepository, &out.CommitMessage, &out.CommitAuthorName, &out.CommitAuthorLogin,
		&out.CommitAuthorAvatar, &out.CommitTimestamp, &out.CommitIsDefaultBranch)
	return
}

func (s *Store) SelectSeriesPage(ctx context.Context, p storage.SeriesListParams) (out []storage.SeriesPageRow, err error) {
	if p.Fingerprint == nil && p.ActiveSince == nil && p.ActiveUntil == nil {
		return s.catalogSeriesPage(ctx, p)
	}
	filters := `($1::VARCHAR IS NULL OR cs.name ILIKE '%'||$1||'%' OR cs.tags ILIKE '%'||$1||'%')
AND ($2::VARCHAR IS NULL OR m.machine_name=$2) AND ($3::VARCHAR IS NULL OR m.commit_repo_url=$3)`
	cursor := `($7::TIMESTAMP IS NULL OR (m.commit_timestamp,m.history_fingerprint)<($7::TIMESTAMP,$8::VARCHAR))`
	memberFilter, pageFilter := filters, cursor
	latest := `SELECT history_fingerprint,arg_max(id,(commit_timestamp,id)) AS id,
max(commit_timestamp) AS commit_timestamp FROM scoped GROUP BY history_fingerprint`
	pageJoin := ""
	if p.Fingerprint != nil {
		// An exact fingerprint cursor selects its preceding history member.
		memberFilter, pageFilter = cursor, filters
		latest = `SELECT member.* FROM (
SELECT arg_max(scoped,(commit_timestamp,id)) AS member FROM scoped GROUP BY history_fingerprint)`
		pageJoin = ` JOIN "case" cs ON cs.id=m.case_id`
	}
	err = s.read(ctx, func(q queryer) error {
		source := "SELECT * FROM benchmark_result"
		if p.Fingerprint != nil {
			source, err = s.seriesSource(ctx, q, []string{*p.Fingerprint})
			if err != nil {
				return err
			}
		}
		// A shared facts CTE can materialize even with NOT MATERIALIZED.
		// Repeat the source so each scan can project and filter independently.
		// Broad pages retain only the winning ID and timestamp per series; full
		// metadata in every aggregate state exceeds the read handle memory limit.
		// The explicit ID filter bounds the final payload join's build side.
		membership := `SELECT br.id,br.history_fingerprint,
br.case_id,br.context_id,br.hardware_id,br.commit_repo_url,
c.sha AS commit_sha,c."timestamp" AS commit_timestamp,hw.name AS machine_name
FROM (` + source + `) br JOIN "commit" c ON c.id=br.commit_id JOIN hardware hw ON hw.id=br.hardware_id
WHERE ` + historyMembership
		out, err = collectRows(ctx, q, `WITH scoped AS (
SELECT m.* FROM (`+membership+`) m JOIN "case" cs ON cs.id=m.case_id
WHERE `+memberFilter+`
AND ($4::VARCHAR IS NULL OR m.history_fingerprint=$4)
AND ($5::TIMESTAMP IS NULL OR m.commit_timestamp>=$5) AND ($6::TIMESTAMP IS NULL OR m.commit_timestamp<=$6)),
latest AS (`+latest+`),
page AS (
SELECT m.* FROM latest m`+pageJoin+` WHERE `+pageFilter+`
ORDER BY m.commit_timestamp DESC,m.history_fingerprint DESC LIMIT $9),
counts AS (SELECT history_fingerprint,count(*) AS point_count FROM (`+membership+`) membership
WHERE history_fingerprint IN(SELECT history_fingerprint FROM page) GROUP BY history_fingerprint)
SELECT p.history_fingerprint,p.id,br."timestamp",c.sha,p.commit_timestamp,br.commit_repo_url,
br.unit,br.data,cnt.point_count,cs.name,cs.tags,ctx.tags,hw.id,hw.name,hw.type,hw.hash
FROM page p JOIN counts cnt ON cnt.history_fingerprint=p.history_fingerprint
JOIN (SELECT id,"timestamp",unit,data,case_id,context_id,hardware_id,commit_id,commit_repo_url
FROM (`+source+`) WHERE id IN(SELECT id FROM page)) br ON br.id=p.id
JOIN "commit" c ON c.id=br.commit_id JOIN "case" cs ON cs.id=br.case_id
JOIN context ctx ON ctx.id=br.context_id JOIN hardware hw ON hw.id=br.hardware_id
ORDER BY p.commit_timestamp DESC,p.history_fingerprint DESC`,
			[]any{p.Q, p.Hardware, p.Repository, p.Fingerprint, p.ActiveSince, p.ActiveUntil, p.CursorTs, p.CursorFp, p.PageSize}, scanSeriesPage)
		return err
	})
	return
}
