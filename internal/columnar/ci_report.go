package columnar

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

const ciRunColumns = `br.run_id,br.run_tags,br.run_reason,br.commit_repo_url,
c.id,c.sha,c.repository,c.parent,c.fork_point_sha,c."timestamp"`

func scanCIRun(row rowScanner) (out storage.CIReportRunRow, err error) {
	err = row.Scan(&out.RunID, &out.RunTags, &out.RunReason, &out.CommitRepoURL, &out.CommitID,
		&out.CommitSha, &out.CommitRepository, &out.CommitParent, &out.CommitForkPointSha, &out.CommitTimestamp)
	return
}

func (s *Store) SelectCIReportRunsByCommit(ctx context.Context, repository, sha string) ([]storage.CIReportRunRow, error) {
	return readRows(ctx, s, `SELECT DISTINCT `+ciRunColumns+`
FROM benchmark_result br JOIN "commit" c ON c.id=br.commit_id
WHERE c.repository=$1 AND c.sha=$2 ORDER BY br.run_id`, []any{repository, sha}, scanCIRun)
}

func (s *Store) SelectCIReportRunsByIDs(ctx context.Context, runIDs []string) ([]storage.CIReportRunRow, error) {
	return readRows(ctx, s, `SELECT DISTINCT `+ciRunColumns+`
FROM benchmark_result br LEFT JOIN "commit" c ON c.id=br.commit_id
WHERE br.run_id IN (SELECT unnest($1::VARCHAR[])) ORDER BY br.run_id`, []any{runIDs}, scanCIRun)
}

const ciCommitColumns = `id,sha,repository,parent,fork_point_sha,"timestamp",message`

func scanCICommit(row rowScanner) (out storage.CIReportCommitRow, err error) {
	err = row.Scan(&out.CommitID, &out.CommitSha, &out.Repository, &out.Parent, &out.ForkPointSha, &out.Timestamp, &out.Message)
	return
}

// Finalization must select baseline metadata from the same snapshot as facts.
// postgres_query keeps these indexed lookups in PostgreSQL, including recursion.
func (s *Store) GetCIReportCommit(ctx context.Context, repository, sha string) (storage.CIReportCommitRow, error) {
	if s.tx == nil {
		return s.Store.GetCIReportCommit(ctx, repository, sha)
	}
	query := `SELECT ` + ciCommitColumns + ` FROM "commit" WHERE repository=` + literal(repository) + ` AND sha=` + literal(sha)
	return readOne(ctx, s, `SELECT * FROM postgres_query('pg',`+literal(query)+`)`, nil, scanCICommit)
}

func (s *Store) SelectLatestDefaultCommit(ctx context.Context, repository string) (storage.CIReportCommitRow, error) {
	if s.tx == nil {
		return s.Store.SelectLatestDefaultCommit(ctx, repository)
	}
	query := `SELECT ` + ciCommitColumns + ` FROM "commit" WHERE repository=` + literal(repository) + `
AND sha=fork_point_sha AND "timestamp" IS NOT NULL ORDER BY "timestamp" DESC,sha DESC LIMIT 1`
	return readOne(ctx, s, `SELECT * FROM postgres_query('pg',`+literal(query)+`)`, nil, scanCICommit)
}

func (s *Store) SelectCIReportBaselineAncestry(ctx context.Context, repository, sha string, limit int32) ([]storage.CIReportCommitRow, error) {
	if s.tx == nil {
		return s.Store.SelectCIReportBaselineAncestry(ctx, repository, sha, limit)
	}
	query := fmt.Sprintf(`WITH RECURSIVE ancestry AS (
SELECT id,sha,repository,parent,fork_point_sha,"timestamp",message,1 AS depth
FROM "commit" WHERE repository=%s AND sha=%s AND %d>0
UNION ALL
SELECT c.id,c.sha,c.repository,c.parent,c.fork_point_sha,c."timestamp",c.message,a.depth+1
FROM ancestry a JOIN "commit" c ON c.repository=a.repository AND c.sha=a.parent
WHERE a.depth<%d)
SELECT `+ciCommitColumns+` FROM ancestry ORDER BY depth`, literal(repository), literal(sha), limit, limit)
	return readRows(ctx, s, `SELECT * FROM postgres_query('pg',`+literal(query)+`)`, nil, scanCICommit)
}

func ciRunKeys(runs []storage.CIReportRunKey) ([]string, []string) {
	ids, commits := make([]string, len(runs)), make([]string, len(runs))
	for i, run := range runs {
		ids[i], commits[i] = run.RunID, run.CommitID
	}
	return ids, commits
}

func (s *Store) CountCIReportRows(ctx context.Context, runs []storage.CIReportRunKey) (int64, error) {
	ids, commits := ciRunKeys(runs)
	return readOne(ctx, s, `WITH selected AS (
SELECT unnest($1::VARCHAR[]) AS run_id,unnest($2::VARCHAR[]) AS commit_id)
SELECT count(*) FROM benchmark_result br JOIN selected s ON s.run_id=br.run_id AND s.commit_id=br.commit_id`, []any{ids, commits}, scanCount)
}

func (s *Store) SelectCIReportRows(ctx context.Context, runs, baselineRuns []storage.CIReportRunKey) ([]storage.CIReportResultRow, error) {
	ids, commits := ciRunKeys(runs)
	baselineIDs, baselineCommits := ciRunKeys(baselineRuns)
	// Carry only selected runs' payloads into the dimension joins. Joining
	// selected IDs back to the archive lets remote cardinality estimates turn
	// a small report into a payload hash build over every archived result.
	return readRows(ctx, s, `WITH contenders AS (
SELECT unnest($1::VARCHAR[]) AS run_id,unnest($2::VARCHAR[]) AS commit_id),
baselines AS (SELECT unnest($3::VARCHAR[]) AS run_id,unnest($4::VARCHAR[]) AS commit_id),
contender_results AS MATERIALIZED (
SELECT br.id,br.run_id,br."timestamp",br.history_fingerprint,br.case_id,br.context_id,br.info_id,
br.hardware_id,br.commit_id,br.unit,br.data,br.error,br.change_annotations,br.optional_benchmark_info
FROM benchmark_result br JOIN contenders s ON s.run_id=br.run_id AND s.commit_id=br.commit_id),
baseline_members AS (
SELECT br.id,br.run_id,br."timestamp",br.history_fingerprint,br.case_id,br.context_id,br.info_id,
br.hardware_id,br.commit_id,br.unit,br.data,br.error,br.change_annotations,br.optional_benchmark_info
FROM benchmark_result br JOIN baselines s ON s.run_id=br.run_id AND s.commit_id=br.commit_id
WHERE br.history_fingerprint IN (SELECT history_fingerprint FROM contender_results)),
baseline_results AS (
SELECT member.* FROM (
SELECT arg_max(baseline_members,("timestamp",id)) AS member
FROM baseline_members GROUP BY run_id,commit_id,history_fingerprint)),
selected AS MATERIALIZED (SELECT *,0 AS side_order FROM contender_results UNION ALL SELECT *,1 AS side_order FROM baseline_results)
SELECT br.id,br.run_id,br."timestamp",br.history_fingerprint,cs.name,cs.tags,ctx.tags,inf.tags,
hw.id,hw.type,hw.name,hw.hash,c.id,c.sha,c.repository,c.parent,c.fork_point_sha,c."timestamp",
br.unit,br.data,br.error,br.change_annotations,br.optional_benchmark_info
FROM selected br
JOIN "case" cs ON cs.id=br.case_id JOIN context ctx ON ctx.id=br.context_id
JOIN info inf ON inf.id=br.info_id JOIN hardware hw ON hw.id=br.hardware_id
LEFT JOIN "commit" c ON c.id=br.commit_id
ORDER BY br.side_order,br.run_id,br.history_fingerprint,br."timestamp",br.id`,
		[]any{ids, commits, baselineIDs, baselineCommits}, func(row rowScanner) (out storage.CIReportResultRow, err error) {
			err = row.Scan(&out.ResultID, &out.RunID, &out.ResultTimestamp, &out.HistoryFingerprint,
				&out.CaseName, &out.CaseTags, &out.ContextTags, &out.InfoTags, &out.HardwareID, &out.HardwareType,
				&out.HardwareName, &out.HardwareHash, &out.CommitID, &out.CommitSha, &out.CommitRepository,
				&out.CommitParent, &out.CommitForkPointSha, &out.CommitTimestamp, &out.Unit,
				nullableFloats{&out.Data}, &out.Error, &out.ChangeAnnotations, &out.OptionalBenchmarkInfo)
			return
		})
}

func (s *Store) SelectLatestAlertRun(ctx context.Context, p storage.SelectLatestAlertRunParams) (storage.AlertCandidateRun, error) {
	return readOne(ctx, s, `SELECT br.run_id,c.sha,br."timestamp"
FROM benchmark_result br JOIN "commit" c ON c.id=br.commit_id
WHERE br.commit_repo_url=$1 AND ($2::VARCHAR IS NULL OR br.run_reason=$2)
ORDER BY br."timestamp" DESC,br.id DESC LIMIT 1`, []any{p.Repository, p.RunReason}, func(row rowScanner) (out storage.AlertCandidateRun, err error) {
		err = row.Scan(&out.RunID, &out.CommitSHA, &out.LastResultTimestamp)
		return
	})
}

func (s *Store) SelectRepositoryVerdictRuns(ctx context.Context, repository string, since time.Time) ([]string, error) {
	return readRows(ctx, s, `SELECT DISTINCT br.run_id FROM benchmark_result br
LEFT JOIN "commit" c ON c.id=br.commit_id WHERE br.commit_repo_url=$1 AND br."timestamp">=$2
AND (c.id IS NULL OR c.sha IS DISTINCT FROM c.fork_point_sha)`, []any{repository, since}, scanString)
}

func (s *Store) SelectRunVerdictSubjects(ctx context.Context, runID string) ([]storage.RunVerdictSubject, error) {
	return readRows(ctx, s, `SELECT br.commit_repo_url,c.sha,coalesce(c.sha=c.fork_point_sha,false),br."timestamp",br.id
FROM benchmark_result br LEFT JOIN "commit" c ON c.id=br.commit_id WHERE br.run_id=$1
QUALIFY row_number() OVER (PARTITION BY br.commit_repo_url ORDER BY br."timestamp" DESC,br.id DESC)=1
ORDER BY br.commit_repo_url`, []any{runID}, func(row rowScanner) (out storage.RunVerdictSubject, err error) {
		err = row.Scan(&out.Repository, &out.CommitSHA, &out.DefaultBranch, &out.LastResultAt, &out.LastResultID)
		return
	})
}

func (s *Store) SelectCommitVerdictSubjects(ctx context.Context, commitID string) ([]storage.CommitVerdictSubject, error) {
	return readRows(ctx, s, `SELECT DISTINCT run_id,benchmark_id,commit_repo_url FROM benchmark_result WHERE commit_id=$1`, []any{commitID}, func(row rowScanner) (out storage.CommitVerdictSubject, err error) {
		err = row.Scan(&out.RunID, &out.BenchmarkID, &out.Repository)
		return
	})
}
