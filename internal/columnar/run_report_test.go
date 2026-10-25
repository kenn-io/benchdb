package columnar

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestFinalizedReportKeepsCommitEnrichmentOutsideSnapshot(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "sum", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "fixture", Hash: "fixture"})
	require.NoError(t, err)
	const repository = "https://example.com/repo"
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// Comparisons need ten earlier default-branch results.
	for i := range 7 {
		sha := fmt.Sprintf("earlier-%d", i)
		at := start.AddDate(0, 0, i-7)
		commitID, err := pg.GetOrCreateCommit(ctx, storage.InsertCommitParams{
			Repository: repository, Sha: sha, ForkPointSha: new(sha), Timestamp: new(at),
		})
		require.NoError(t, err)
		_, err = pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
			CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
			RunID: sha, RunTags: []byte(`{}`), CommitID: &commitID, CommitRepoUrl: repository,
			HistoryFingerprint: "fingerprint", Timestamp: at,
			Unit: new("s"), Data: []*float64{new(10.0 + float64(i%3)), new(11.0 + float64(i%3))},
		})
		require.NoError(t, err)
	}
	var lateID, resultID string
	for i, commit := range []storage.InsertCommitParams{
		{Sha: "c1", ForkPointSha: new("c1"), Timestamp: new(start)},
		{Sha: "c2", Parent: new("c1"), ForkPointSha: new("c2"), Timestamp: new(start.Add(24 * time.Hour))},
		{Sha: "c3", Parent: new("c2"), ForkPointSha: new("c3"), Timestamp: new(start.Add(48 * time.Hour))},
		{Sha: "late"},
		{Sha: "contender", Parent: new("c3"), ForkPointSha: new("c3"), Timestamp: new(start.Add(96 * time.Hour))},
	} {
		commit.Repository = repository
		commitID, err := pg.GetOrCreateCommit(ctx, commit)
		require.NoError(t, err)
		id, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
			CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
			RunID: commit.Sha, RunTags: []byte(`{}`), CommitID: &commitID, CommitRepoUrl: repository,
			HistoryFingerprint: "fingerprint", Timestamp: start.Add(time.Duration(i) * 24 * time.Hour),
			Unit: new("s"), Data: []*float64{new(10.0 + float64(i)), new(11.0 + float64(i))},
		})
		require.NoError(t, err)
		if commit.Sha == "late" {
			lateID = commitID
		}
		if commit.Sha == "contender" {
			resultID = id
		}
	}
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	var hot int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&hot))
	require.Zero(t, hot, "the report must read archived measurements")
	store := NewStore(pg, e)
	query := service.CIReportQuery{RunIDs: []string{"contender"}, Baseline: service.CIReportBaselineLatestDefault}
	before, err := service.NewCIReporter(store).Report(ctx, query)
	require.NoError(t, err)
	require.Len(t, before.Comparisons, 1)
	require.Equal(t, "c3", before.Runs[0].BaselineCommit.Sha)
	require.NotNil(t, before.Comparisons[0].Analysis.LookbackZScore)

	_, err = store.FinalizeRunReport(ctx, "contender", []string{resultID}, func(ctx context.Context, transaction storage.Store) (storage.RunReport, error) {
		// Finalization has pinned its manifest and facts. The normal repair
		// writer commits enrichment on another connection before analysis starts.
		updated, err := pg.UpdateUnknownCommit(ctx, storage.UpdateUnknownCommitParams{
			ID: lateID, Parent: new("c3"), ForkPointSha: new("late"),
			Timestamp: start.Add(72 * time.Hour), Message: "enriched baseline",
		})
		require.NoError(t, err)
		require.EqualValues(t, 1, updated)
		metadata, err := transaction.GetCIReportCommit(ctx, repository, "late")
		require.NoError(t, err)
		assert.Nil(t, metadata.Timestamp, "commit lookup must see the report's snapshot")
		ancestry, err := transaction.SelectCIReportBaselineAncestry(ctx, repository, "late", 20)
		require.NoError(t, err)
		assert.Len(t, ancestry, 1, "the newly discovered parent belongs to a later snapshot")
		latest, err := transaction.SelectLatestDefaultCommit(ctx, repository)
		require.NoError(t, err)
		assert.Equal(t, "c3", latest.CommitSha)
		report, err := service.NewCIReporter(transaction).Report(ctx, query)
		if err != nil {
			return storage.RunReport{}, err
		}
		report.BaselineRunID = report.Runs[0].BaselineRunID
		data, err := json.Marshal(report)
		return storage.RunReport{Report: data, Summary: []byte(`{}`), EvaluatedAt: time.Now().UTC()}, err
	})
	require.NoError(t, err)
	saved, err := service.NewRunReports(store, "").Get(ctx, "contender", 1)
	require.NoError(t, err)
	require.Len(t, saved.Runs, 1)
	assert.Equal(t, "c3", saved.Runs[0].BaselineCommit.Sha)
	require.Len(t, saved.Runs[0].Comparisons, 1)
	require.NotNil(t, saved.Runs[0].Comparisons[0].Analysis)
	assert.NotNil(t, saved.Runs[0].Comparisons[0].Analysis.LookbackZScore, "a finalized report must retain its lookback analysis")

	// The service must validate archived measurements, not just hot rows or
	// the report's rendered comparisons, before saving a new evaluation.
	reports := service.NewRunReports(store, "")
	revised, err := reports.Reevaluate(ctx, "contender")
	require.NoError(t, err)
	assert.Equal(t, 2, revised.Evaluation)
	latest, err := reports.Get(ctx, "contender", 0)
	require.NoError(t, err)
	require.Len(t, latest.Runs[0].Comparisons, 1)
	assert.Equal(t, resultID, latest.Runs[0].Comparisons[0].Contender.ResultID)
	require.NotNil(t, latest.BaselineRunID)
	assert.Equal(t, "c3", *latest.BaselineRunID)
	original, err := store.GetRunReport(ctx, "contender", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{resultID}, original.ResultIDs)

	fresh, err := service.NewCIReporter(store).Report(ctx, query)
	require.NoError(t, err)
	assert.Equal(t, "late", fresh.Runs[0].BaselineCommit.Sha, "later reports see completed enrichment")
	require.NotNil(t, fresh.Comparisons[0].Analysis.LookbackZScore)

	require.NoError(t, store.DeleteBenchmarkResult(ctx, resultID))
	_, err = reports.Reevaluate(ctx, "contender")
	require.ErrorIs(t, err, storage.ErrConflict)
	latest, err = reports.Get(ctx, "contender", 0)
	require.NoError(t, err)
	assert.Equal(t, 2, latest.Evaluation, "missing archived measurements must not create another evaluation")
}
