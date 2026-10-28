package columnar

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

// SelectBenchmarkResults reads one run's files for a run filter. Batch, reason,
// and time filters have no index, so an unfiltered list reads every file.
func (s *Store) SelectBenchmarkResults(ctx context.Context, p storage.ListResultsParams) ([]storage.ResultListRow, error) {
	scope := queryScope{AllFacts: true}
	if p.RunID != nil {
		scope = queryScope{RunIDs: []string{*p.RunID}}
	}
	return readRows(ctx, s, scope, `SELECT br.id,br.run_id,br.run_reason,br.run_tags,br.batch_id,br."timestamp",
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

func (s *Store) SelectRecentRuns(ctx context.Context, p storage.RecentRunsParams) (out []storage.RecentRunRow, err error) {
	if p.Search == "" && !p.NeedsAttention {
		return s.recentRunPage(ctx, p)
	}
	out = []storage.RecentRunRow{}
	err = s.pinned(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		var matching []string
		if p.Search != "" {
			var err error
			if matching, err = searchedRuns(ctx, snapshot, p); err != nil || len(matching) == 0 {
				return err
			}
		}
		selected, err := recentCandidatePage(ctx, snapshot, p, matching)
		if err != nil || len(selected) == 0 {
			return err
		}
		if err := snapshot.selectFacts(ctx, queryScope{RunIDs: selected}); err != nil {
			return err
		}
		out, err = collectRows(ctx, snapshot, "WITH "+fmt.Sprintf(recentSummarySQL, `SELECT `+recentColumns+` FROM benchmark_result
WHERE ($1::VARCHAR IS NULL OR commit_repo_url=$1) AND run_id IN (SELECT unnest($2::VARCHAR[]))`), []any{p.Repository, selected}, scanRecentRun)
		return err
	})
	return
}

// searchedRuns returns the runs with a result at a commit whose URL matches
// the search. Matching commits come from PostgreSQL; their archived results
// are found through commit membership.
func searchedRuns(ctx context.Context, snapshot *ReadSnapshot, p storage.RecentRunsParams) ([]string, error) {
	pattern := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(p.Search) + "%"
	commits, err := collectRows(ctx, snapshot, `SELECT id FROM "commit" c
WHERE (rtrim(c.repository,'/')||'/commit/'||c.sha) ILIKE $1 ESCAPE '!'`, []any{pattern}, scanString)
	if err != nil || len(commits) == 0 {
		return nil, err
	}
	if err := snapshot.selectFacts(ctx, queryScope{CommitIDs: commits}); err != nil {
		return nil, err
	}
	return collectRows(ctx, snapshot, `SELECT DISTINCT run_id FROM benchmark_result
WHERE ($1::VARCHAR IS NULL OR commit_repo_url=$1) AND commit_id IN (SELECT unnest($2::VARCHAR[]))`, []any{p.Repository, commits}, scanString)
}

// recentCandidatePage orders candidate runs by their last result and returns
// the requested page. Attention candidates and their times come from the
// verdict table; a search's matching runs are aggregated from their facts.
func recentCandidatePage(ctx context.Context, snapshot *ReadSnapshot, p storage.RecentRunsParams, matching []string) ([]string, error) {
	if p.NeedsAttention {
		query := `SELECT v.run_id FROM run_verdict v WHERE v.needs_attention
AND ($1::VARCHAR IS NULL OR v.repository=$1)
AND ($1::VARCHAR IS NOT NULL OR NOT EXISTS(SELECT 1 FROM run_verdict newer WHERE newer.run_id=v.run_id
AND (newer.last_result_at,newer.last_result_id)>(v.last_result_at,v.last_result_id)))`
		args := []any{p.Repository, p.Offset, p.PageSize}
		if p.Search != "" {
			query += ` AND v.run_id IN (SELECT unnest($4::VARCHAR[]))`
			args = append(args, matching)
		}
		return collectRows(ctx, snapshot, query+` ORDER BY v.last_result_at DESC,v.run_id DESC LIMIT $3 OFFSET $2`, args, scanString)
	}
	if err := snapshot.selectFacts(ctx, queryScope{RunIDs: matching}); err != nil {
		return nil, err
	}
	return collectRows(ctx, snapshot, `SELECT run_id FROM benchmark_result
WHERE ($1::VARCHAR IS NULL OR commit_repo_url=$1) AND run_id IN (SELECT unnest($2::VARCHAR[]))
GROUP BY run_id ORDER BY max("timestamp") DESC,run_id DESC LIMIT $4 OFFSET $3`, []any{p.Repository, matching, p.Offset, p.PageSize}, scanString)
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

// seriesWindowCandidatesSQL selects the series that may have an eligible
// member in the window and match the page's case, machine, and repository
// filters, the same filters seriesPageSQL applies. Summaries bound each
// series' eligible commit times; mutated and stale series are candidates
// whatever their summaries say, when they match the filters. A series whose
// facts are all hot has no summary and needs none: hot facts are always read.
const seriesWindowCandidatesSQL = `WITH matching AS (
SELECT sc.* FROM archive_series_catalog sc
JOIN "case" cs ON cs.id=sc.case_id JOIN hardware hw ON hw.id=sc.hardware_id
WHERE ($3::VARCHAR IS NULL OR cs.name ILIKE '%'||$3||'%' OR cs.tags ILIKE '%'||$3||'%')
AND ($4::VARCHAR IS NULL OR hw.name=$4) AND ($5::VARCHAR IS NULL OR sc.commit_repo_url=$5))
SELECT DISTINCT history_fingerprint FROM matching
WHERE point_count>0 AND ($1::TIMESTAMP IS NULL OR commit_timestamp>=$1)
AND ($2::TIMESTAMP IS NULL OR first_commit_timestamp<=$2)
UNION SELECT history_fingerprint FROM matching
WHERE history_fingerprint IN (SELECT history_fingerprint FROM (` + affectedSeriesSQL + `))`

// seriesPageScope names an exact series, or every series that may be active
// in a time-filtered page.
func seriesPageScope(ctx context.Context, snapshot *ReadSnapshot, p storage.SeriesListParams) (queryScope, error) {
	if p.Fingerprint != nil {
		return queryScope{Fingerprints: []string{*p.Fingerprint}}, nil
	}
	if err := snapshot.seriesSummaries(ctx); err != nil {
		return queryScope{}, err
	}
	fingerprints, err := collectRows(ctx, snapshot, seriesWindowCandidatesSQL,
		[]any{p.ActiveSince, p.ActiveUntil, p.Q, p.Hardware, p.Repository}, scanString)
	return queryScope{Fingerprints: fingerprints}, err
}

func (s *Store) SelectSeriesPage(ctx context.Context, p storage.SeriesListParams) (out []storage.SeriesPageRow, err error) {
	if p.Fingerprint == nil && p.ActiveSince == nil && p.ActiveUntil == nil {
		return s.catalogSeriesPage(ctx, p)
	}
	out = []storage.SeriesPageRow{}
	err = s.pinned(ctx, func(ctx context.Context, q *ReadSnapshot) error {
		scope, err := seriesPageScope(ctx, q, p)
		if err != nil {
			return err
		}
		if err := q.selectFacts(ctx, scope); err != nil {
			return err
		}
		page, err := collectRows(ctx, q, seriesPageSQL(p), []any{p.Q, p.Hardware, p.Repository, p.Fingerprint,
			p.ActiveSince, p.ActiveUntil, p.CursorTs, p.CursorFp, p.PageSize}, func(row rowScanner) (out seriesPageEntry, err error) {
			err = row.Scan(&out.fingerprint, &out.id, &out.commitTimestamp)
			return
		})
		if err != nil || len(page) == 0 {
			return err
		}
		values := make([]string, len(page))
		fingerprints := make([]string, len(page))
		for i, entry := range page {
			fingerprints[i] = entry.fingerprint
			values[i] = fmt.Sprintf("(%s,%s,TIMESTAMP %s)", literal(entry.fingerprint), literal(entry.id),
				literal(entry.commitTimestamp.UTC().Format("2006-01-02 15:04:05.999999")))
		}
		// Counts include each page series' members outside the window, which
		// may be in files the window's candidates did not select.
		if p.Fingerprint == nil {
			if err := q.selectFacts(ctx, queryScope{Fingerprints: fingerprints}); err != nil {
				return err
			}
		}
		out, err = collectRows(ctx, q, `WITH page(history_fingerprint,id,commit_timestamp) AS (VALUES `+strings.Join(values, ",")+`),
counts AS (SELECT history_fingerprint,count(*) AS point_count FROM (`+seriesMembershipSQL+`) membership
WHERE history_fingerprint IN(SELECT history_fingerprint FROM page) GROUP BY history_fingerprint)
SELECT p.history_fingerprint,p.id,br."timestamp",c.sha,p.commit_timestamp,br.commit_repo_url,
br.unit,br.data,cnt.point_count,cs.name,cs.tags,ctx.tags,hw.id,hw.name,hw.type,hw.hash
FROM page p JOIN counts cnt ON cnt.history_fingerprint=p.history_fingerprint
JOIN (SELECT id,"timestamp",unit,data,case_id,context_id,hardware_id,commit_id,commit_repo_url
FROM benchmark_result WHERE id IN(SELECT id FROM page)) br ON br.id=p.id
JOIN "commit" c ON c.id=br.commit_id JOIN "case" cs ON cs.id=br.case_id
JOIN context ctx ON ctx.id=br.context_id JOIN hardware hw ON hw.id=br.hardware_id
ORDER BY p.commit_timestamp DESC,p.history_fingerprint DESC`, nil, scanSeriesPage)
		return err
	})
	return
}

type seriesPageEntry struct {
	fingerprint, id string
	commitTimestamp time.Time
}

// A shared facts CTE can materialize even with NOT MATERIALIZED. Repeat the
// membership so each scan can project and filter independently. Broad pages
// retain only the winning ID and timestamp per series; full metadata in every
// aggregate state exceeds the read handle memory limit.
const seriesMembershipSQL = `SELECT br.id,br.history_fingerprint,
br.case_id,br.context_id,br.hardware_id,br.commit_repo_url,
c.sha AS commit_sha,c."timestamp" AS commit_timestamp,hw.name AS machine_name
FROM benchmark_result br JOIN "commit" c ON c.id=br.commit_id JOIN hardware hw ON hw.id=br.hardware_id
WHERE ` + historyMembership

// seriesPageSQL selects a page of series whose members match the filters:
// each series' fingerprint, newest matching member, and its commit time.
func seriesPageSQL(p storage.SeriesListParams) string {
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
	return `WITH scoped AS (
SELECT m.* FROM (` + seriesMembershipSQL + `) m JOIN "case" cs ON cs.id=m.case_id
WHERE ` + memberFilter + `
AND ($4::VARCHAR IS NULL OR m.history_fingerprint=$4)
AND ($5::TIMESTAMP IS NULL OR m.commit_timestamp>=$5) AND ($6::TIMESTAMP IS NULL OR m.commit_timestamp<=$6)),
latest AS (` + latest + `)
SELECT m.history_fingerprint,m.id,m.commit_timestamp FROM latest m` + pageJoin + ` WHERE ` + pageFilter + `
ORDER BY m.commit_timestamp DESC,m.history_fingerprint DESC LIMIT $9`
}
