package columnar

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

// The PostgreSQL adapter is the independent oracle. Every check is repeated
// after physical eviction, so accidentally inheriting its fact reads fails.
func TestAnalyticalReadsMatchPostgresAcrossArchive(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	target := summary.ProductSmoke
	baseline, err := pg.GetBenchmarkResultByID(ctx, target.BaselineResultID)
	require.NoError(t, err)
	oddID, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: baseline.CaseID, ContextID: baseline.ContextID, InfoID: baseline.InfoID, HardwareID: baseline.HardwareID,
		RunID: "nullable-samples", RunTags: []byte(`{"nested":{"flag":false}}`), CommitID: baseline.CommitID,
		CommitRepoUrl: baseline.CommitRepoUrl, HistoryFingerprint: baseline.HistoryFingerprint,
		Timestamp: baseline.Timestamp.Add(-time.Hour), Unit: baseline.Unit, Mean: new(1e100),
		Data: []*float64{new(1e100), nil, new(0.0)}, Times: []*float64{}, Error: []byte(`null`),
		OptionalBenchmarkInfo: []byte(`false`), Validation: []byte(`null`), ChangeAnnotations: []byte(`{"exclude_from_history":false}`),
		SubmissionKey: new("columnar-parity-key"), SubmissionPayloadSHA256: new(strings.Repeat("a", 64)),
	})
	require.NoError(t, err)
	for i, annotations := range [][]byte{nil, []byte(`null`), []byte(`{"exclude_from_history":false}`), []byte(`{"exclude_from_history":true}`), []byte(`{"exclude_from_history":"true"}`)} {
		_, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
			CaseID: baseline.CaseID, ContextID: baseline.ContextID, InfoID: baseline.InfoID, HardwareID: baseline.HardwareID,
			RunID: "annotation-values", RunTags: []byte(`{}`), CommitID: baseline.CommitID,
			CommitRepoUrl: baseline.CommitRepoUrl, HistoryFingerprint: baseline.HistoryFingerprint,
			Timestamp: baseline.Timestamp.Add(time.Duration(i) * time.Minute), Unit: baseline.Unit,
			Data: []*float64{new(1.25)}, ChangeAnnotations: annotations,
			OptionalBenchmarkInfo: []byte(`{"tolerance":{"absolute":0.5,"relative_percent":10}}`),
		})
		require.NoError(t, err)
	}
	oldCommitAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	oldCommitID, err := pg.GetOrCreateCommit(ctx, storage.InsertCommitParams{
		Sha: "old-commit", Repository: "https://example.test/old-repository", Timestamp: &oldCommitAt,
		ForkPointSha: new("old-commit"),
	})
	require.NoError(t, err)
	oldResultID, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: baseline.CaseID, ContextID: baseline.ContextID, InfoID: baseline.InfoID, HardwareID: baseline.HardwareID,
		RunID: "old-commit-new-result", RunTags: []byte(`{}`), CommitID: &oldCommitID,
		CommitRepoUrl: "https://example.test/old-repository", HistoryFingerprint: "old-repository-fingerprint",
		Timestamp: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC), Unit: baseline.Unit, Data: []*float64{new(1.25)},
	})
	require.NoError(t, err)
	tiedResultID, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: baseline.CaseID, ContextID: baseline.ContextID, InfoID: baseline.InfoID, HardwareID: baseline.HardwareID,
		RunID: "old-commit-tied-result", RunTags: []byte(`{}`), CommitID: &oldCommitID,
		CommitRepoUrl: "https://example.test/old-repository", HistoryFingerprint: "old-repository-fingerprint",
		Timestamp: oldCommitAt.Add(time.Hour), Unit: baseline.Unit, Data: []*float64{new(2.5)},
	})
	require.NoError(t, err)
	tiedSeries, err := pg.SelectSeriesPage(ctx, storage.SeriesListParams{Fingerprint: new("old-repository-fingerprint"), PageSize: 1})
	require.NoError(t, err)
	require.Len(t, tiedSeries, 1)
	assert.Equal(t, max(oldResultID, tiedResultID), tiedSeries[0].LatestResultID)
	assert.Equal(t, int64(2), tiedSeries[0].PointCount)
	commitlessID, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: baseline.CaseID, ContextID: baseline.ContextID, InfoID: baseline.InfoID, HardwareID: baseline.HardwareID,
		RunID: "commitless-run", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.test/commitless",
		HistoryFingerprint: "commitless-fingerprint", Timestamp: oldCommitAt, Unit: baseline.Unit, Data: []*float64{new(1.25)},
	})
	require.NoError(t, err)
	verdicts := service.NewVerdicts(pg, 0)
	for {
		claimed, err := verdicts.Process(ctx)
		require.NoError(t, err)
		if claimed == 0 {
			break
		}
	}
	contender, err := pg.GetBenchmarkResultByID(ctx, target.ContenderResultID)
	require.NoError(t, err)
	benchmarkID := ""
	require.NoError(t, pool.QueryRow(ctx, `SELECT benchmark_id FROM benchmark_result WHERE id=$1`, baseline.ID).Scan(&benchmarkID))
	contenderKeys := []storage.CIReportRunKey{{RunID: contender.RunID, CommitID: *contender.CommitID}}
	baselineKeys := []storage.CIReportRunKey{{RunID: baseline.RunID, CommitID: *baseline.CommitID}}
	asOf := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	series, err := pg.SelectSeriesPage(ctx, storage.SeriesListParams{Fingerprint: &summary.Fingerprint, PageSize: 1})
	require.NoError(t, err)
	require.Len(t, series, 1)
	seriesCursor := series[0].LatestCommitTimestamp
	benchmarks, err := pg.SelectBenchmarkList(ctx, storage.BenchmarkListParams{PageSize: 1})
	require.NoError(t, err)
	require.Len(t, benchmarks.Rows, 1)
	benchmarkCursor := storage.BenchmarkCursor{Status: benchmarks.Rows[0].Status, Name: benchmarks.Rows[0].CaseName, ID: benchmarks.Rows[0].BenchmarkID}
	runIDs := []string{target.RecentRunID, contender.RunID, baseline.RunID}
	checks := []struct {
		name string
		read func(storage.Store) (any, error)
	}{
		{"count", func(st storage.Store) (any, error) { return st.CountBenchmarkResults(ctx) }},
		{"result", func(st storage.Store) (any, error) { return st.GetBenchmarkResultByID(ctx, oddID) }},
		{"detail", func(st storage.Store) (any, error) { return st.GetBenchmarkResultDetail(ctx, oddID) }},
		{"commitless-detail", func(st storage.Store) (any, error) { return st.GetBenchmarkResultDetail(ctx, commitlessID) }},
		{"compare", func(st storage.Store) (any, error) { return st.GetResultForCompare(ctx, oddID) }},
		{"replay", func(st storage.Store) (any, error) {
			return st.GetBenchmarkResultBySubmissionKey(ctx, "columnar-parity-key")
		}},
		{"history", func(st storage.Store) (any, error) { return st.SelectHistoryForFingerprint(ctx, summary.Fingerprint) }},
		{"history-asof", func(st storage.Store) (any, error) {
			return st.SelectHistoryForFingerprintsAsOf(ctx, []string{summary.Fingerprint}, asOf)
		}},
		{"benchmark-history", func(st storage.Store) (any, error) { return st.SelectHistoryForBenchmark(ctx, benchmarkID) }},
		{"series-members", func(st storage.Store) (any, error) {
			return st.SelectSeriesMembers(ctx, storage.SeriesMembersParams{Fingerprints: []string{summary.Fingerprint}})
		}},
		{"series", func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{PageSize: 100})
		}},
		{"series-tied-commit", func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{Fingerprint: new("old-repository-fingerprint"), PageSize: 1})
		}},
		{"series-search", func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{Q: new("ingest"), ActiveUntil: &asOf, PageSize: 100})
		}},
		{"series-fingerprint-display-filter", func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{Fingerprint: &summary.Fingerprint, Hardware: new("missing-machine"), PageSize: 1})
		}},
		{"series-active-window-count", func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{ActiveSince: &seriesCursor, PageSize: 100})
		}},
		{"series-fingerprint-cursor", func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{Fingerprint: &summary.Fingerprint, CursorTs: &seriesCursor, CursorFp: &summary.Fingerprint, PageSize: 1})
		}},
		{"series-cursor", func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{CursorTs: &seriesCursor, CursorFp: &summary.Fingerprint, PageSize: 2})
		}},
		{"list", func(st storage.Store) (any, error) {
			return st.SelectBenchmarkResults(ctx, storage.ListResultsParams{PageSize: 100})
		}},
		{"list-page", func(st storage.Store) (any, error) {
			return st.SelectBenchmarkResults(ctx, storage.ListResultsParams{Cursor: &target.LatestResultID, PageSize: 3})
		}},
		{"recent", func(st storage.Store) (any, error) {
			return st.SelectRecentRuns(ctx, storage.RecentRunsParams{PageSize: 100})
		}},
		{"recent-page", func(st storage.Store) (any, error) {
			return st.SelectRecentRuns(ctx, storage.RecentRunsParams{Repository: &target.Repository, Offset: 2, PageSize: 3})
		}},
		{"recent-search", func(st storage.Store) (any, error) {
			return st.SelectRecentRuns(ctx, storage.RecentRunsParams{Search: "commit-", PageSize: 100})
		}},
		{"recent-attention", func(st storage.Store) (any, error) {
			return st.SelectRecentRuns(ctx, storage.RecentRunsParams{NeedsAttention: true, PageSize: 100})
		}},
		{"recent-absent", func(st storage.Store) (any, error) {
			return st.SelectRecentRuns(ctx, storage.RecentRunsParams{Repository: new("https://example.test/absent"), PageSize: 100})
		}},
		{"repositories", func(st storage.Store) (any, error) { return st.SelectRepositories(ctx) }},
		{"ci-runs", func(st storage.Store) (any, error) { return st.SelectCIReportRunsByIDs(ctx, runIDs) }},
		{"ci-commit-runs", func(st storage.Store) (any, error) {
			return st.SelectCIReportRunsByCommit(ctx, target.Repository, target.CIRegressionCommitSHA)
		}},
		{"ci-commit", func(st storage.Store) (any, error) {
			return st.GetCIReportCommit(ctx, target.Repository, target.CIRegressionCommitSHA)
		}},
		{"ci-latest-commit", func(st storage.Store) (any, error) { return st.SelectLatestDefaultCommit(ctx, target.Repository) }},
		{"ci-ancestry", func(st storage.Store) (any, error) {
			return st.SelectCIReportBaselineAncestry(ctx, target.Repository, target.CIRegressionCommitSHA, 10)
		}},
		{"ci-count", func(st storage.Store) (any, error) { return st.CountCIReportRows(ctx, contenderKeys) }},
		{"ci-results", func(st storage.Store) (any, error) { return st.SelectCIReportRows(ctx, contenderKeys, baselineKeys) }},
		{"ci-results-overlap", func(st storage.Store) (any, error) { return st.SelectCIReportRows(ctx, baselineKeys, baselineKeys) }},
		{"ci-results-empty", func(st storage.Store) (any, error) { return st.SelectCIReportRows(ctx, nil, baselineKeys) }},
		{"run-subjects", func(st storage.Store) (any, error) { return st.SelectRunVerdictSubjects(ctx, target.RecentRunID) }},
		{"run-verdicts", func(st storage.Store) (any, error) { return st.SelectRunVerdicts(ctx, runIDs) }},
		{"pending", func(st storage.Store) (any, error) { return st.VerdictsPending(ctx, &target.Repository) }},
		{"attention-count", func(st storage.Store) (any, error) { return st.CountAttentionRuns(ctx, &target.Repository) }},
		{"repository-runs", func(st storage.Store) (any, error) {
			runs, err := st.SelectRepositoryVerdictRuns(ctx, target.Repository, time.Time{})
			slices.Sort(runs)
			return runs, err
		}},
		{"commit-subjects", func(st storage.Store) (any, error) {
			subjects, err := st.SelectCommitVerdictSubjects(ctx, *baseline.CommitID)
			slices.SortFunc(subjects, func(a, b storage.CommitVerdictSubject) int {
				return strings.Compare(a.RunID+"/"+a.BenchmarkID+"/"+a.Repository, b.RunID+"/"+b.BenchmarkID+"/"+b.Repository)
			})
			return subjects, err
		}},
		{"benchmarks", func(st storage.Store) (any, error) {
			return st.SelectBenchmarkList(ctx, storage.BenchmarkListParams{PageSize: 100})
		}},
		{"benchmark-window", func(st storage.Store) (any, error) {
			return st.SelectBenchmarkList(ctx, storage.BenchmarkListParams{PageSize: 100, ActiveSince: &baseline.Timestamp, ActiveUntil: &asOf})
		}},
		{"benchmark-cursor", func(st storage.Store) (any, error) {
			return st.SelectBenchmarkList(ctx, storage.BenchmarkListParams{PageSize: 1, Cursor: &benchmarkCursor})
		}},
		{"benchmark-status", func(st storage.Store) (any, error) {
			return st.SelectBenchmarkList(ctx, storage.BenchmarkListParams{PageSize: 1, Status: new("stable")})
		}},
	}
	expected := make([]any, len(checks))
	for i, check := range checks {
		expected[i], err = check.read(pg)
		require.NoError(t, err, check.name)
	}
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, BatchRows: 7, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	analytical := NewStore(pg, e)
	for _, phase := range []string{"hot", "mixed", "archived"} {
		if phase != "hot" {
			for {
				stats, err := e.Maintain(ctx)
				require.NoError(t, err)
				if phase == "mixed" {
					require.EqualValues(t, 7, stats.ExportedRows)
					break
				}
				if stats.ExportedRows == 0 {
					break
				}
			}
		}
		t.Run(phase, func(t *testing.T) {
			for i, check := range checks {
				t.Run(check.name, func(t *testing.T) {
					actual, err := check.read(analytical)
					require.NoError(t, err)
					if want, ok := expected[i].(storage.BenchmarkList); ok {
						got := actual.(storage.BenchmarkList)
						require.Len(t, got.Rows, len(want.Rows))
						want.Rows = slices.Clone(want.Rows)
						for j := range want.Rows {
							assert.JSONEq(t, string(want.Rows[j].PreviewTracks), string(got.Rows[j].PreviewTracks))
							want.Rows[j].PreviewTracks, got.Rows[j].PreviewTracks = nil, nil
						}
						assert.Equal(t, want, got)
					} else {
						assert.Equal(t, expected[i], actual)
					}
				})
			}
			_, err := analytical.GetBenchmarkResultByID(ctx, "missing-result")
			assert.ErrorIs(t, err, storage.ErrNotFound)
		})
	}
	var retained int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&retained))
	assert.Zero(t, retained, "parity must include actual PostgreSQL eviction")
	require.NoError(t, pg.EnqueueVerdict(ctx, storage.VerdictKindCommit, *baseline.CommitID))
	pending, err := analytical.VerdictsPending(ctx, &target.Repository)
	require.NoError(t, err)
	assert.True(t, pending, "commit repair keeps archived runs pending before its queue entry expands")
	pending, err = analytical.VerdictsPending(ctx, new("https://example.test/old-repository"))
	require.NoError(t, err)
	assert.False(t, pending)
	runs, err := analytical.SelectRunVerdicts(ctx, runIDs)
	require.NoError(t, err)
	require.NotEmpty(t, runs)
	for _, run := range runs {
		assert.True(t, run.Pending)
	}
	benchmarks, err = analytical.SelectBenchmarkList(ctx, storage.BenchmarkListParams{Repository: &target.Repository, PageSize: 1})
	require.NoError(t, err)
	assert.True(t, benchmarks.Pending)
	require.NoError(t, analytical.DeleteBenchmarkResult(ctx, commitlessID))
	var remainingMembership int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM run_membership WHERE run_id='commitless-run'`).Scan(&remainingMembership))
	assert.Zero(t, remainingMembership)
	pending, err = analytical.VerdictsPending(ctx, new("https://example.test/commitless"))
	require.NoError(t, err)
	assert.True(t, pending, "the deleted run's stored verdict stays pending until recomputation removes it")
}
